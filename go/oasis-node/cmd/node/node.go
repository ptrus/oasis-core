// Package node implements the Oasis node.
package node

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/crash"
	"github.com/oasisprotocol/oasis-core/go/common/grpc"
	"github.com/oasisprotocol/oasis-core/go/common/identity"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/persistent"
	"github.com/oasisprotocol/oasis-core/go/common/version"
	consensusAPI "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint"
	controlAPI "github.com/oasisprotocol/oasis-core/go/control/api"
	genesisAPI "github.com/oasisprotocol/oasis-core/go/genesis/api"
	governanceAPI "github.com/oasisprotocol/oasis-core/go/governance/api"
	"github.com/oasisprotocol/oasis-core/go/ias"
	iasAPI "github.com/oasisprotocol/oasis-core/go/ias/api"
	keymanagerAPI "github.com/oasisprotocol/oasis-core/go/keymanager/api"
	cmdCommon "github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/common"
	"github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/common/background"
	"github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/common/flags"
	cmdGrpc "github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/common/grpc"
	registryAPI "github.com/oasisprotocol/oasis-core/go/registry/api"
	roothashAPI "github.com/oasisprotocol/oasis-core/go/roothash/api"
	runtimeRegistry "github.com/oasisprotocol/oasis-core/go/runtime/registry"
	scheduler "github.com/oasisprotocol/oasis-core/go/scheduler/api"
	"github.com/oasisprotocol/oasis-core/go/sentry"
	sentryAPI "github.com/oasisprotocol/oasis-core/go/sentry/api"
	stakingAPI "github.com/oasisprotocol/oasis-core/go/staking/api"
	storageAPI "github.com/oasisprotocol/oasis-core/go/storage/api"
	"github.com/oasisprotocol/oasis-core/go/upgrade"
	upgradeAPI "github.com/oasisprotocol/oasis-core/go/upgrade/api"
	workerBeacon "github.com/oasisprotocol/oasis-core/go/worker/beacon"
	workerClient "github.com/oasisprotocol/oasis-core/go/worker/client"
	workerCommon "github.com/oasisprotocol/oasis-core/go/worker/common"
	"github.com/oasisprotocol/oasis-core/go/worker/common/p2p"
	p2pAPI "github.com/oasisprotocol/oasis-core/go/worker/common/p2p/api"
	lightP2P "github.com/oasisprotocol/oasis-core/go/worker/common/p2p/light"
	"github.com/oasisprotocol/oasis-core/go/worker/compute/executor"
	workerConsensusRPC "github.com/oasisprotocol/oasis-core/go/worker/consensusrpc"
	workerKeymanager "github.com/oasisprotocol/oasis-core/go/worker/keymanager"
	"github.com/oasisprotocol/oasis-core/go/worker/registration"
	workerSentry "github.com/oasisprotocol/oasis-core/go/worker/sentry"
	workerStorage "github.com/oasisprotocol/oasis-core/go/worker/storage"
)

const exportsSubDir = "exports"

// Node is the Oasis node service.
//
// WARNING: This is exposed for the benefit of tests and the interface
// is not guaranteed to be stable.
type Node struct {
	svcMgr       *background.ServiceManager
	grpcInternal *grpc.Server

	stopOnce sync.Once

	commonStore *persistent.CommonStore

	Consensus consensusAPI.Backend

	Upgrader upgradeAPI.Backend
	Genesis  genesisAPI.Provider
	Identity *identity.Identity
	Sentry   sentryAPI.LocalBackend
	IAS      iasAPI.Endpoint

	RuntimeRegistry runtimeRegistry.Registry

	CommonWorker       *workerCommon.Worker
	ExecutorWorker     *executor.Worker
	StorageWorker      *workerStorage.Worker
	ClientWorker       *workerClient.Worker
	SentryWorker       *workerSentry.Worker
	P2P                p2pAPI.Service
	RegistrationWorker *registration.Worker
	KeymanagerWorker   *workerKeymanager.Worker
	ConsensusWorker    *workerConsensusRPC.Worker
	BeaconWorker       *workerBeacon.Worker
	readyCh            chan struct{}

	logger *logging.Logger
}

// Cleanup cleans up after the node has terminated.
func (n *Node) Cleanup() {
	n.svcMgr.Cleanup()
	if n.Upgrader != nil {
		n.Upgrader.Close()
	}
	if n.commonStore != nil {
		n.commonStore.Close()
	}
}

// Stop gracefully terminates the node.
func (n *Node) Stop() {
	n.stopOnce.Do(func() {
		n.svcMgr.Stop()
	})
}

// Wait waits for the node to gracefully terminate.  Callers MUST
// call Cleanup() after wait returns.
func (n *Node) Wait() {
	n.svcMgr.Wait()
}

func (n *Node) waitReady() {
	if err := n.WaitSync(context.Background()); err != nil {
		n.logger.Error("failed while waiting for node consensus sync", "err", err)
		return
	}

	// Wait for client worker.
	if n.ClientWorker.Enabled() {
		<-n.ClientWorker.Initialized()
	}

	// Wait for storage worker.
	if n.StorageWorker.Enabled() {
		<-n.StorageWorker.Initialized()
	}

	// Wait for executor worker (also waits runtimes to initialize).
	if n.ExecutorWorker.Enabled() {
		<-n.ExecutorWorker.Initialized()
	}

	// Wait for key manager worker.
	if n.KeymanagerWorker.Enabled() {
		<-n.KeymanagerWorker.Initialized()
	}

	// Wait for the common worker.
	if n.CommonWorker.Enabled() {
		<-n.CommonWorker.Initialized()
	}

	close(n.readyCh)
}

// startRuntimeServices initializes and starts all the services that are required for runtime
// support to work.
func (n *Node) startRuntimeServices() error {
	var err error
	if n.Sentry, err = sentry.New(n.Consensus, n.Identity); err != nil {
		return err
	}

	// Initialize and register the internal gRPC services.
	grpcSrv := n.grpcInternal.Server()
	beacon.RegisterService(grpcSrv, n.Consensus.Beacon())
	scheduler.RegisterService(grpcSrv, n.Consensus.Scheduler())
	registryAPI.RegisterService(grpcSrv, n.Consensus.Registry())
	stakingAPI.RegisterService(grpcSrv, n.Consensus.Staking())
	keymanagerAPI.RegisterService(grpcSrv, n.Consensus.KeyManager())
	roothashAPI.RegisterService(grpcSrv, n.Consensus.RootHash())
	governanceAPI.RegisterService(grpcSrv, n.Consensus.Governance())

	// Register dump genesis halt hook.
	n.Consensus.RegisterHaltHook(func(ctx context.Context, blockHeight int64, epoch beacon.EpochTime, _ error) {
		n.logger.Info("Consensus halt hook: dumping genesis",
			"epoch", epoch,
			"block_height", blockHeight+1,
		)
		if err = n.dumpGenesis(ctx, blockHeight+1, epoch); err != nil {
			n.logger.Error("halt hook: failed to dump genesis",
				"err", err,
			)
			return
		}
		n.logger.Info("Consensus halt hook: genesis dumped",
			"epoch", epoch,
			"block_height", blockHeight+1,
		)
	})

	// Initialize runtime workers.
	if err = n.initRuntimeWorkers(); err != nil {
		n.logger.Error("failed to initialize workers",
			"err", err,
		)
		return err
	}

	// Start workers (requires NodeController for checking, if nodes are synced).
	if err = n.startRuntimeWorkers(); err != nil {
		n.logger.Error("failed to start workers",
			"err", err,
		)
		return err
	}

	n.logger.Debug("runtime services started")

	return nil
}

func (n *Node) initRuntimeWorkers() error {
	dataDir := cmdCommon.DataDir()

	var err error

	genesisDoc, err := n.Genesis.GetGenesisDocument()
	if err != nil {
		return err
	}

	if genesisDoc.Registry.Parameters.DebugAllowUnroutableAddresses {
		p2p.DebugForceAllowUnroutableAddresses()
	}
	n.P2P, err = p2p.New(n.Identity, n.Consensus)
	if err != nil {
		return err
	}
	// Register light blocks P2P service.
	n.P2P.RegisterProtocolServer(lightP2P.NewServer(n.Consensus))
	n.svcMgr.Register(n.P2P)

	// Initialize the IAS proxy client.
	n.IAS, err = ias.New(n.Identity)
	if err != nil {
		n.logger.Error("failed to initialize IAS proxy client",
			"err", err,
		)
		return err
	}

	// Initialize the node's runtime registry.
	n.RuntimeRegistry, err = runtimeRegistry.New(n.svcMgr.Ctx, cmdCommon.DataDir(), n.Consensus, n.IAS)
	if err != nil {
		return err
	}
	n.svcMgr.RegisterCleanupOnly(n.RuntimeRegistry, "runtime registry")

	// Initialize the common worker.
	n.CommonWorker, err = workerCommon.New(
		n,
		dataDir,
		n.Identity,
		n.Consensus,
		n.P2P,
		n.IAS,
		n.Consensus.KeyManager(),
		n.RuntimeRegistry,
		genesisDoc,
	)
	if err != nil {
		n.logger.Error("failed to initialize common worker",
			"err", err,
		)
		return err
	}
	n.svcMgr.Register(n.CommonWorker.Grpc)
	n.svcMgr.Register(n.CommonWorker)

	workerCommonCfg := n.CommonWorker.GetConfig()

	// Initialize the registration worker.
	n.RegistrationWorker, err = registration.New(
		dataDir,
		n.Consensus.Beacon(),
		n.Consensus.Registry(),
		n.Identity,
		n.Consensus,
		n.P2P,
		&workerCommonCfg,
		n.commonStore,
		n, // the delegate to be called on registration shutdown
		n.RuntimeRegistry,
	)
	if genesisDoc.Registry.Parameters.DebugAllowUnroutableAddresses {
		registration.DebugForceAllowUnroutableAddresses()
	}
	if err != nil {
		n.logger.Error("failed to initialize worker registration",
			"err", err,
		)
		return err
	}
	n.svcMgr.Register(n.RegistrationWorker)

	// Initialize the beacon worker.
	n.BeaconWorker, err = workerBeacon.New(
		n.Identity,
		n.Consensus,
		n.commonStore,
		n.RegistrationWorker,
	)
	if err != nil {
		return err
	}
	n.svcMgr.Register(n.BeaconWorker)

	// Initialize the storage worker.
	n.StorageWorker, err = workerStorage.New(
		n.grpcInternal,
		n.CommonWorker,
		n.RegistrationWorker,
		n.Genesis,
	)
	if err != nil {
		return err
	}
	n.svcMgr.Register(n.StorageWorker)

	// Initialize the key manager worker.
	n.KeymanagerWorker, err = workerKeymanager.New(
		dataDir,
		n.CommonWorker,
		n.IAS,
		n.RegistrationWorker,
		n.Consensus.KeyManager(),
	)
	if err != nil {
		return err
	}
	n.svcMgr.Register(n.KeymanagerWorker)

	// Initialize the executor worker.
	n.ExecutorWorker, err = executor.New(
		n.CommonWorker,
		n.RegistrationWorker,
	)
	if err != nil {
		return err
	}
	n.svcMgr.Register(n.ExecutorWorker)

	// Initialize the client worker.
	n.ClientWorker, err = workerClient.New(n.grpcInternal, n.CommonWorker)
	if err != nil {
		return err
	}
	n.svcMgr.Register(n.ClientWorker)

	// Commit storage settings to the registered runtimes.
	err = n.RuntimeRegistry.FinishInitialization(n.svcMgr.Ctx)
	if err != nil {
		return err
	}

	// Initialize the sentry worker.
	n.SentryWorker, err = workerSentry.New(
		n.Sentry,
		n.Identity,
	)
	if err != nil {
		return err
	}
	n.svcMgr.Register(n.SentryWorker)

	// Initialize the public consensus services worker.
	n.ConsensusWorker, err = workerConsensusRPC.New(n.CommonWorker, n.RegistrationWorker)
	if err != nil {
		return err
	}
	n.svcMgr.Register(n.ConsensusWorker)

	return nil
}

func (n *Node) startRuntimeWorkers() error {
	// Start the common worker.
	if err := n.CommonWorker.Start(); err != nil {
		return err
	}

	// Start the runtime client worker.
	if err := n.ClientWorker.Start(); err != nil {
		return err
	}

	// Start the storage worker.
	if err := n.StorageWorker.Start(); err != nil {
		return err
	}

	// Start the executor worker.
	if err := n.ExecutorWorker.Start(); err != nil {
		return err
	}

	// Start the key manager worker.
	if err := n.KeymanagerWorker.Start(); err != nil {
		return err
	}

	// Start the worker registration service.
	if err := n.RegistrationWorker.Start(); err != nil {
		return err
	}

	// Start the beacon worker.
	if err := n.BeaconWorker.Start(); err != nil {
		return err
	}

	// Start the sentry worker.
	if err := n.SentryWorker.Start(); err != nil {
		return err
	}

	// Start the public consensus services worker.
	if err := n.ConsensusWorker.Start(); err != nil {
		return fmt.Errorf("consensus worker: %w", err)
	}

	// Only start the external gRPC server if needed.
	if n.ConsensusWorker.Enabled() {
		if err := n.CommonWorker.Grpc.Start(); err != nil {
			n.logger.Error("failed to start external gRPC server",
				"err", err,
			)
			return err
		}
	}

	// Close readyCh once all workers and runtimes are initialized.
	go n.waitReady()

	return nil
}

func (n *Node) dumpGenesis(ctx context.Context, blockHeight int64, epoch beacon.EpochTime) error {
	doc, err := n.Consensus.StateToGenesis(ctx, blockHeight)
	if err != nil {
		return fmt.Errorf("dumpGenesis: failed to get genesis: %w", err)
	}

	exportsDir := filepath.Join(cmdCommon.DataDir(), exportsSubDir)

	if err := common.Mkdir(exportsDir); err != nil {
		return fmt.Errorf("dumpGenesis: failed to create exports dir: %w", err)
	}

	filename := filepath.Join(exportsDir, fmt.Sprintf("genesis-%s-at-%d.json", doc.ChainID, doc.Height))
	if err := doc.WriteFileJSON(filename); err != nil {
		return fmt.Errorf("dumpGenesis: failed to write genesis file: %w", err)
	}

	return nil
}

// NewNode initializes and launches the Oasis node service.
//
// WARNING: This will misbehave iff cmd != RootCommand().  This is exposed
// for the benefit of tests and the interface is not guaranteed to be stable.
//
// Note: the reason for having the named err return value here is for the
// deferred func below to propagate the error.
func NewNode() (node *Node, err error) { // nolint: gocyclo
	logger := cmdCommon.Logger()

	node = &Node{
		svcMgr:  background.NewServiceManager(logger),
		readyCh: make(chan struct{}),
		logger:  logger,
	}

	// Cleanup on error.
	defer func(node *Node) {
		if err == nil {
			return
		}
		if cErr := node.svcMgr.Ctx.Err(); cErr != nil {
			err = cErr
		}
		node.Stop()
		node.Cleanup()
	}(node)

	// Initialize the common environment.
	if err = initCommon(); err != nil {
		return nil, err
	}

	// Log the version of the binary so that we can figure out what the
	// binary is from the logs.
	logger.Info("Starting oasis-node",
		"Version", version.SoftwareVersion,
	)

	if err = verifyElevatedPrivileges(logger); err != nil {
		return nil, err
	}

	// Initialize the genesis provider.
	node.Genesis, err = initGenesis(logger)
	if err != nil {
		return nil, err
	}

	// Configure a directory for the node to work in.
	dataDir, err := configureDataDir(logger)
	if err != nil {
		return nil, err
	}

	// Generate or load the node's identity.
	node.Identity, err = loadOrGenerateIdentity(dataDir, logger)
	if err != nil {
		return nil, err
	}

	// Load configured values for all registered crash points.
	crash.LoadViperArgValues()

	// Initialize and start the metrics reporting server.
	if _, err = startMetricServer(node.svcMgr, logger); err != nil {
		return nil, err
	}

	// Initialize and start the profiling server.
	if _, err = startProfilingServer(node.svcMgr, logger); err != nil {
		return nil, err
	}

	// Initialize the internal gRPC server.
	node.grpcInternal, err = cmdGrpc.NewServerLocal(false)
	if err != nil {
		logger.Error("failed to initialize internal gRPC server",
			"err", err,
		)
		return nil, err
	}
	node.svcMgr.Register(node.grpcInternal)

	// Register the node as a node controller.
	controlAPI.RegisterService(node.grpcInternal.Server(), node)

	// Open the common node store.
	node.commonStore, err = persistent.NewCommonStore(dataDir)
	if err != nil {
		logger.Error("failed to open common node store",
			"err", err,
		)
		return nil, err
	}

	// Initialize upgrader backend.
	tmMode, err := tendermint.Mode()
	if err != nil {
		logger.Error("invalid tendermint mode",
			"err", err,
		)
		return nil, err
	}
	isArchive := tmMode == consensusAPI.ModeArchive
	node.Upgrader, err = upgrade.New(node.commonStore, cmdCommon.DataDir(), !isArchive)
	if err != nil {
		logger.Error("failed to initialize upgrade backend",
			"err", err,
		)
		return nil, err
	}
	// If not an archive mode, check if we can even launch.
	if !isArchive {
		if err = node.Upgrader.StartupUpgrade(); err != nil {
			logger.Error("error occurred during startup upgrade",
				"err", err,
			)
			return nil, err
		}
	}

	// Initialize Tendermint consensus backend.
	node.Consensus, err = tendermint.New(node.svcMgr.Ctx, dataDir, node.Identity, node.Upgrader, node.Genesis)
	if err != nil {
		logger.Error("failed to initialize tendermint service",
			"err", err,
		)
		return nil, err
	}
	node.svcMgr.Register(node.Consensus)
	consensusAPI.RegisterService(node.grpcInternal.Server(), node.Consensus)

	// If the consensus backend supports communicating with consensus services, we can also start
	// all services required for runtime operation.
	if node.Consensus.SupportedFeatures().Has(consensusAPI.FeatureServices) {
		if err = node.startRuntimeServices(); err != nil {
			logger.Error("failed to initialize runtime services",
				"err", err,
			)
			return nil, err
		}

		if flags.DebugDontBlameOasis() {
			// Register the node as a debug controller if we are in debug mode.
			controlAPI.RegisterDebugService(node.grpcInternal.Server(), node)

			// Enable direct storage access if we are in debug mode.
			storageAPI.RegisterService(node.grpcInternal.Server(), &debugStorage{node})
		}
	}

	// Start the internal gRPC server.
	if err = node.grpcInternal.Start(); err != nil {
		logger.Error("failed to start internal gRPC server",
			"err", err,
		)
		return nil, err
	}

	// Start the consensus backend service.
	if err = node.Consensus.Start(); err != nil {
		logger.Error("failed to start consensus backend service",
			"err", err,
		)
		return nil, err
	}

	logger.Info("initialization complete: ready to serve")

	return node, nil
}
