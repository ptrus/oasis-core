//! Runtime side of the worker-host protocol.
use std::{
    collections::{BTreeMap, HashMap},
    io::{BufReader, BufWriter, Read, Write},
    sync::{
        atomic::{AtomicUsize, Ordering},
        Arc, Mutex,
    },
};

use byteorder::{BigEndian, ReadBytesExt, WriteBytesExt};
use crossbeam::channel;
use io_context::Context;
use slog::{error, info, warn, Logger};
use thiserror::Error;

use crate::{
    common::{logger::get_logger, namespace::Namespace, version::Version},
    consensus::{
        tendermint,
        verifier::{TrustRoot, Verifier},
    },
    dispatcher::Dispatcher,
    rak::RAK,
    storage::KeyValue,
    types::{Body, Error, Message, MessageType, RuntimeInfoRequest, RuntimeInfoResponse},
    BUILD_INFO,
};

#[cfg(not(target_env = "sgx"))]
pub type Stream = ::std::os::unix::net::UnixStream;
#[cfg(target_env = "sgx")]
pub type Stream = ::std::net::TcpStream;

/// Maximum message size.
const MAX_MESSAGE_SIZE: usize = 16 * 1024 * 1024; // 16MiB

#[derive(Error, Debug)]
pub enum ProtocolError {
    #[error("message too large")]
    MessageTooLarge,
    #[error("method not supported")]
    MethodNotSupported,
    #[error("invalid response")]
    InvalidResponse,
    #[error("attestation required")]
    #[allow(unused)]
    AttestationRequired,
    #[error("host environment information not configured")]
    HostInfoNotConfigured,
    #[error("incompatible consensus backend")]
    IncompatibleConsensusBackend,
    #[error("invalid runtime id (expected: {0} got: {1})")]
    InvalidRuntimeId(Namespace, Namespace),
    #[error("already initialized")]
    AlreadyInitialized,
    #[error("channel closed")]
    ChannelClosed,
}

impl From<ProtocolError> for Error {
    fn from(err: ProtocolError) -> Self {
        Self {
            module: "protocol".to_string(),
            code: 1,
            message: err.to_string(),
        }
    }
}

/// Information about the host environment.
#[derive(Debug, Clone)]
pub struct HostInfo {
    /// Assigned runtime identifier of the loaded runtime.
    pub runtime_id: Namespace,
    /// Name of the consensus backend that is in use for the consensus layer.
    pub consensus_backend: String,
    /// Consensus protocol version that is in use for the consensus layer.
    pub consensus_protocol_version: Version,
    /// Consensus layer chain domain separation context.
    pub consensus_chain_context: String,
    /// Node-local runtime configuration.
    ///
    /// This configuration must not be used in any context which requires determinism across
    /// replicated runtime instances.
    pub local_config: BTreeMap<String, cbor::Value>,
}

/// Runtime part of the runtime host protocol.
pub struct Protocol {
    /// Logger.
    logger: Logger,
    /// Runtime attestation key.
    #[cfg_attr(not(target_env = "sgx"), allow(unused))]
    rak: Arc<RAK>,
    /// Incoming request dispatcher.
    dispatcher: Arc<Dispatcher>,
    /// Mutex for sending outgoing messages.
    outgoing_mutex: Mutex<()>,
    /// Stream to the runtime host.
    stream: Stream,
    /// Outgoing request identifier generator.
    last_request_id: AtomicUsize,
    /// Pending outgoing requests.
    pending_out_requests: Mutex<HashMap<u64, channel::Sender<Body>>>,
    /// Runtime version.
    runtime_version: Version,
    /// Consensus layer trust root.
    trust_root: Option<TrustRoot>,
    /// Host environment information.
    host_info: Mutex<Option<HostInfo>>,
}

impl Protocol {
    /// Create a new protocol handler instance.
    pub fn new(
        stream: Stream,
        rak: Arc<RAK>,
        dispatcher: Arc<Dispatcher>,
        runtime_version: Version,
        trust_root: Option<TrustRoot>,
    ) -> Self {
        let logger = get_logger("runtime/protocol");

        Self {
            logger,
            rak,
            dispatcher,
            outgoing_mutex: Mutex::new(()),
            stream,
            last_request_id: AtomicUsize::new(0),
            pending_out_requests: Mutex::new(HashMap::new()),
            runtime_version,
            trust_root,
            host_info: Mutex::new(None),
        }
    }

    /// The runtime identifier for this instance.
    ///
    /// # Panics
    ///
    /// Panics, if the host environment information is not set.
    pub fn get_runtime_id(self: &Protocol) -> Namespace {
        self.host_info
            .lock()
            .unwrap()
            .as_ref()
            .expect("host environment information should be set")
            .runtime_id
    }

    /// The host environment information for this instance.
    ///
    /// # Panics
    ///
    /// Panics, if the host environment information is not set.
    pub fn get_host_info(self: &Protocol) -> HostInfo {
        self.host_info
            .lock()
            .unwrap()
            .as_ref()
            .expect("host environment information should be set")
            .clone()
    }

    /// Start the protocol handler loop.
    pub fn start(self: &Arc<Protocol>) {
        info!(self.logger, "Starting protocol handler");
        let mut reader = BufReader::new(&self.stream);

        'recv: loop {
            match self.handle_message(&mut reader) {
                Err(error) => {
                    error!(self.logger, "Failed to handle message"; "err" => %error);
                    break 'recv;
                }
                Ok(()) => {}
            }
        }

        info!(self.logger, "Protocol handler is terminating");
    }

    /// Make a new request to the worker host and wait for the response.
    pub fn make_request(&self, _ctx: Context, body: Body) -> Result<Body, Error> {
        let id = self.last_request_id.fetch_add(1, Ordering::SeqCst) as u64;
        let message = Message {
            id,
            body,
            message_type: MessageType::Request,
        };

        // Create a response channel and register an outstanding pending request.
        let (tx, rx) = channel::bounded(1);
        {
            let mut pending_requests = self.pending_out_requests.lock().unwrap();
            pending_requests.insert(id, tx);
        }

        // Write message to stream and wait for the response.
        self.encode_message(message).map_err(Error::from)?;

        let result = rx
            .recv()
            .map_err(|_| Error::from(ProtocolError::ChannelClosed))?;
        match result {
            Body::Error(err) => Err(err),
            body => Ok(body),
        }
    }

    /// Send an async response to a previous request back to the worker host.
    pub fn send_response(&self, id: u64, body: Body) -> anyhow::Result<()> {
        self.encode_message(Message {
            id,
            body,
            message_type: MessageType::Response,
        })
    }

    fn decode_message<R: Read>(&self, mut reader: R) -> anyhow::Result<Message> {
        let length = reader.read_u32::<BigEndian>()? as usize;
        if length > MAX_MESSAGE_SIZE {
            return Err(ProtocolError::MessageTooLarge.into());
        }

        // TODO: Avoid allocations.
        let mut buffer = vec![0; length];
        reader.read_exact(&mut buffer)?;

        Ok(cbor::from_slice(&buffer)?)
    }

    fn encode_message(&self, message: Message) -> anyhow::Result<()> {
        let _guard = self.outgoing_mutex.lock().unwrap();
        let mut writer = BufWriter::new(&self.stream);

        let buffer = cbor::to_vec(message);
        if buffer.len() > MAX_MESSAGE_SIZE {
            return Err(ProtocolError::MessageTooLarge.into());
        }

        writer.write_u32::<BigEndian>(buffer.len() as u32)?;
        writer.write_all(&buffer)?;

        Ok(())
    }

    fn handle_message<R: Read>(self: &Arc<Protocol>, reader: R) -> anyhow::Result<()> {
        let message = self.decode_message(reader)?;

        match message.message_type {
            MessageType::Request => {
                // Incoming request.
                let id = message.id;
                let ctx = Context::background();

                let body = match self.handle_request(ctx, id, message.body) {
                    Ok(Some(result)) => result,
                    Ok(None) => {
                        // A message will be sent later by another thread so there
                        // is no need to do anything more.
                        return Ok(());
                    }
                    Err(error) => {
                        Body::Error(Error::new("rhp/dispatcher", 1, &format!("{}", error)))
                    }
                };

                // Send response back.
                self.encode_message(Message {
                    id,
                    message_type: MessageType::Response,
                    body,
                })?;
            }
            MessageType::Response => {
                // Response to our request.
                let response_sender = {
                    let mut pending_requests = self.pending_out_requests.lock().unwrap();
                    pending_requests.remove(&message.id)
                };

                match response_sender {
                    Some(response_sender) => {
                        if let Err(error) = response_sender.try_send(message.body) {
                            warn!(self.logger, "Unable to deliver response to local handler"; "err" => %error);
                        }
                    }
                    None => {
                        warn!(self.logger, "Received response message for unknown request"; "msg_id" => message.id);
                    }
                }
            }
            _ => warn!(self.logger, "Received a malformed message"),
        }

        Ok(())
    }

    fn handle_request(
        self: &Arc<Protocol>,
        ctx: Context,
        id: u64,
        request: Body,
    ) -> anyhow::Result<Option<Body>> {
        match request {
            Body::RuntimeInfoRequest(request) => Ok(Some(Body::RuntimeInfoResponse(
                self.initialize_guest(request)?,
            ))),
            Body::RuntimePingRequest {} => Ok(Some(Body::Empty {})),
            Body::RuntimeShutdownRequest {} => {
                info!(self.logger, "Received worker shutdown request");
                Err(ProtocolError::MethodNotSupported.into())
            }
            req @ Body::RuntimeAbortRequest {} => {
                info!(self.logger, "Received worker abort request");
                self.can_handle_runtime_requests()?;
                self.dispatcher.abort_and_wait(ctx, id, req)?;
                info!(self.logger, "Handled worker abort request");
                Ok(Some(Body::RuntimeAbortResponse {}))
            }
            #[cfg(target_env = "sgx")]
            Body::RuntimeCapabilityTEERakInitRequest { target_info } => {
                info!(self.logger, "Initializing the runtime attestation key");
                self.rak.init_rak(target_info)?;
                Ok(Some(Body::RuntimeCapabilityTEERakInitResponse {}))
            }
            #[cfg(target_env = "sgx")]
            Body::RuntimeCapabilityTEERakReportRequest {} => {
                // Initialize the RAK report (for attestation).
                info!(
                    self.logger,
                    "Initializing the runtime attestation key report"
                );
                let (rak_pub, report, nonce) = self.rak.init_report();

                let report: &[u8] = report.as_ref();
                let report = report.to_vec();

                Ok(Some(Body::RuntimeCapabilityTEERakReportResponse {
                    rak_pub,
                    report,
                    nonce,
                }))
            }
            #[cfg(target_env = "sgx")]
            Body::RuntimeCapabilityTEERakAvrRequest { avr } => {
                info!(
                    self.logger,
                    "Configuring AVR for the runtime attestation key binding"
                );
                self.rak.set_avr(avr)?;
                Ok(Some(Body::RuntimeCapabilityTEERakAvrResponse {}))
            }
            Body::RuntimeRPCCallRequest { .. }
            | Body::RuntimeLocalRPCCallRequest { .. }
            | Body::RuntimeCheckTxBatchRequest { .. }
            | Body::RuntimeExecuteTxBatchRequest { .. }
            | Body::RuntimeKeyManagerPolicyUpdateRequest { .. }
            | Body::RuntimeQueryRequest { .. }
            | Body::RuntimeConsensusSyncRequest { .. } => {
                self.can_handle_runtime_requests()?;
                self.dispatcher.queue_request(ctx, id, request)?;
                Ok(None)
            }
            _ => {
                warn!(self.logger, "Received unsupported request"; "req" => format!("{:?}", request));
                Err(ProtocolError::MethodNotSupported.into())
            }
        }
    }

    fn initialize_guest(
        self: &Arc<Protocol>,
        host_info: RuntimeInfoRequest,
    ) -> anyhow::Result<RuntimeInfoResponse> {
        info!(self.logger, "Received host environment information";
            "runtime_id" => ?host_info.runtime_id,
            "consensus_backend" => &host_info.consensus_backend,
            "consensus_protocol_version" => ?host_info.consensus_protocol_version,
            "consensus_chain_context" => &host_info.consensus_chain_context,
            "local_config" => ?host_info.local_config,
        );

        if tendermint::BACKEND_NAME != &host_info.consensus_backend {
            return Err(ProtocolError::IncompatibleConsensusBackend.into());
        }
        if !BUILD_INFO
            .consensus_version
            .is_compatible_with(&host_info.consensus_protocol_version)
        {
            return Err(ProtocolError::IncompatibleConsensusBackend.into());
        }
        let mut local_host_info = self.host_info.lock().unwrap();
        if local_host_info.is_some() {
            return Err(ProtocolError::AlreadyInitialized.into());
        }

        // Create and start the consensus verifier.
        let consensus_verifier: Box<dyn Verifier> = if let Some(ref trust_root) = self.trust_root {
            // Make sure that the host environment matches the trust root.
            if host_info.runtime_id != trust_root.runtime_id {
                return Err(ProtocolError::InvalidRuntimeId(
                    trust_root.runtime_id,
                    host_info.runtime_id,
                )
                .into());
            }

            // Create the Tendermint consensus layer verifier and spawn it in a separate thread.
            let verifier = tendermint::verifier::Verifier::new(self.clone(), trust_root.clone());
            let handle = verifier.handle();
            verifier.start();

            Box::new(handle)
        } else {
            // Create a no-op verifier.
            Box::new(tendermint::verifier::NopVerifier::new(self.clone()))
        };

        // Configure the host environment info.
        *local_host_info = Some(HostInfo {
            runtime_id: host_info.runtime_id,
            consensus_backend: host_info.consensus_backend,
            consensus_protocol_version: host_info.consensus_protocol_version,
            consensus_chain_context: host_info.consensus_chain_context,
            local_config: host_info.local_config,
        });

        // Start the dispatcher.
        self.dispatcher.start(self.clone(), consensus_verifier);

        Ok(RuntimeInfoResponse {
            protocol_version: BUILD_INFO.protocol_version,
            runtime_version: self.runtime_version,
        })
    }

    fn can_handle_runtime_requests(&self) -> anyhow::Result<()> {
        if self.host_info.lock().unwrap().is_none() {
            return Err(ProtocolError::HostInfoNotConfigured.into());
        }

        #[cfg(target_env = "sgx")]
        {
            if self.rak.avr().is_none() {
                return Err(ProtocolError::AttestationRequired.into());
            }
        }

        Ok(())
    }
}

/// Untrusted key/value store which stores arbitrary binary key/value pairs
/// on the worker host.
///
/// Care MUST be taken to not trust this interface at all.  The worker host
/// is capable of doing whatever it wants including but not limited to,
/// hiding data, tampering with keys/values, ignoring writes, replaying
/// past values, etc.
pub struct ProtocolUntrustedLocalStorage {
    ctx: Arc<Context>,
    protocol: Arc<Protocol>,
}

impl ProtocolUntrustedLocalStorage {
    pub fn new(ctx: Context, protocol: Arc<Protocol>) -> Self {
        Self {
            ctx: ctx.freeze(),
            protocol,
        }
    }
}

impl KeyValue for ProtocolUntrustedLocalStorage {
    fn get(&self, key: Vec<u8>) -> Result<Vec<u8>, Error> {
        let ctx = Context::create_child(&self.ctx);

        match self
            .protocol
            .make_request(ctx, Body::HostLocalStorageGetRequest { key })?
        {
            Body::HostLocalStorageGetResponse { value } => Ok(value),
            _ => Err(ProtocolError::InvalidResponse.into()),
        }
    }

    fn insert(&self, key: Vec<u8>, value: Vec<u8>) -> Result<(), Error> {
        let ctx = Context::create_child(&self.ctx);

        match self
            .protocol
            .make_request(ctx, Body::HostLocalStorageSetRequest { key, value })?
        {
            Body::HostLocalStorageSetResponse {} => Ok(()),
            _ => Err(ProtocolError::InvalidResponse.into()),
        }
    }
}
