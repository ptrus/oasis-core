// Package p2p implements the P2P backed light client provider.
package p2p

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core"
	tmlightprovider "github.com/tendermint/tendermint/light/provider"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/oasisprotocol/oasis-core/go/common/logging"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/light"
	p2plight "github.com/oasisprotocol/oasis-core/go/worker/common/p2p/light"
	p2prpc "github.com/oasisprotocol/oasis-core/go/worker/common/p2p/rpc"
)

var (
	registeredLightClients sync.Map
	peerSelectTimeout      = 1 * time.Second
)

// lightClientProvider implements Tendermint's light client provider interface using the Oasis Core
// light blocks P2P API.
type lightClientProvider struct {
	chainID string

	peerInitCh chan struct{}

	p2pClient p2prpc.Client
	peerID    *core.PeerID

	logger *logging.Logger
}

// Backend implements light.Provider
func (lp *lightClientProvider) Backend() consensus.LightClientBackend {
	// TODO: implement other p2p methods. This should be a wrapper.
	panic("unimplemented")
}

// Implements tmlightprovider.Provider.
func (lp *lightClientProvider) ChainID() string {
	return lp.chainID
}

func (lp *lightClientProvider) selectPeer(ctx context.Context) {
	lp.logger.Debug("selecting peer")

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(peerSelectTimeout):
			peers := lp.p2pClient.GetBestPeers()
			lp.logger.Debug("all peers", "peers", peers)
			for _, peerID := range peers {
				if _, loaded := registeredLightClients.LoadOrStore(&peerID, true); !loaded {
					lp.logger.Debug("peer selected", "peer_id", peerID)
					lp.peerID = &peerID

					close(lp.peerInitCh)

					return
				}
			}

		}
	}
}

// Implements tmlightprovider.Provider.
func (lp *lightClientProvider) LightBlock(ctx context.Context, height int64) (*tmtypes.LightBlock, error) {
	// Ensure peer is initialized.
	select {
	case <-lp.peerInitCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	/*
		provider.ErrNoResponse, provider.ErrLightBlockNotFound, provider.ErrHeightTooHigh -> swap
	*/
	lp.logger.Debug("fetching lightblock", "peer", lp.peerID, "height", height)

	if lp.peerID == nil {
		return nil, tmlightprovider.ErrNoResponse
	}

	var rsp *consensus.LightBlock
	pf, err := lp.p2pClient.Call(ctx, p2plight.MethodGetLightBlock, height, &rsp, p2plight.MaxGetLightBlockResponseTime, p2prpc.WithPeer(*lp.peerID))
	switch {
	case err == nil:
	case errors.Is(err, consensus.ErrVersionNotFound):
		lp.logger.Debug("light block fetch failure", "error", err)
		return nil, tmlightprovider.ErrLightBlockNotFound
	default:
		lp.logger.Debug("light block fetch failure", "error", err)
		return nil, tmlightprovider.ErrNoResponse
	}

	lp.logger.Debug("light block received", "pf", pf, "rsp", rsp, "err", err, "height", height)
	// Ensure peer returned the block for the queried height.
	if rsp.Height != height {
		lp.logger.Debug("light block invalid block height", "height", height, "block_height", rsp.Height)
		pf.RecordBadPeer()
		return nil, tmlightprovider.ErrBadLightBlock{Reason: fmt.Errorf("invalid block height: requested: %d, received: %d", height, rsp.Height)}
	}

	// Decode Tendermint-specific light block.
	var protoLb tmproto.LightBlock
	if err = protoLb.Unmarshal(rsp.Meta); err != nil {
		lp.logger.Debug("light block unmarshal error", "err", err, "rsp", rsp)
		pf.RecordBadPeer()
		return nil, tmlightprovider.ErrBadLightBlock{Reason: err}
	}
	tlb, err := tmtypes.LightBlockFromProto(&protoLb)
	if err != nil {
		lp.logger.Debug("light block unmarshal from proto error", "err", err, "rsp", rsp)
		pf.RecordBadPeer()
		return nil, tmlightprovider.ErrBadLightBlock{Reason: err}
	}
	if err = tlb.ValidateBasic(lp.chainID); err != nil {
		lp.logger.Debug("light block validate basic error", "err", err, "rsp", rsp)
		pf.RecordFailure()
		return nil, tmlightprovider.ErrBadLightBlock{Reason: err}
	}

	lp.logger.Debug("light block valid", "height", height)
	return tlb, nil
}

// Implements tmlightprovider.Provider.
func (lp *lightClientProvider) ReportEvidence(ctx context.Context, ev tmtypes.Evidence) error {
	// TODO: not implemented at the moment.
	return fmt.Errorf("not implemented")
}

// NewLightClientProvider creates a new provider for the Tendermint's light client backed by the
// Oasis Core LightBlocks P2P protocol.
//
// Each of the created providers will be backed by its own, single P2P Peer.
//
// The provided chain ID must be the Tendermint chain ID.
func NewLightClientProvider(
	ctx context.Context,
	chainID string,
	p2p p2prpc.Client,
) (light.Provider, error) {
	p := &lightClientProvider{
		chainID:    chainID,
		peerInitCh: make(chan struct{}),
		p2pClient:  p2p,
		logger:     logging.GetLogger("tendermint/light/p2p"),
	}

	go p.selectPeer(ctx)

	return p, nil
}
