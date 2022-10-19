package light

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/spf13/viper"

	tmlight "github.com/tendermint/tendermint/light"
	tmlightprovider "github.com/tendermint/tendermint/light/provider"
	tmlightdb "github.com/tendermint/tendermint/light/store/db"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	tmtypes "github.com/tendermint/tendermint/types"
	tmdb "github.com/tendermint/tm-db"

	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/consensus/api/transaction"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/common"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/light"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/light/client/grpc"
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/light/client/p2p"
	"github.com/oasisprotocol/oasis-core/go/storage/mkvs/syncer"
	p2prpc "github.com/oasisprotocol/oasis-core/go/worker/common/p2p/rpc"
)

type lightClient struct {
	// tmc is the Tendermint light client used for verifying headers.
	tmc *tmlight.Client
}

// Implements consensus.LightClientBackend.
func (lc *lightClient) GetLightBlock(ctx context.Context, height int64) (*consensus.LightBlock, error) {
	return lc.getPrimary().GetLightBlock(ctx, height)
}

// Implements consensus.LightClientBackend.
func (lc *lightClient) GetLightBlockForState(ctx context.Context, height int64) (*consensus.LightBlock, error) {
	return lc.getPrimary().GetLightBlockForState(ctx, height)
}

// Implements consensus.LightClientBackend.
func (lc *lightClient) GetParameters(ctx context.Context, height int64) (*consensus.Parameters, error) {
	return lc.getPrimary().GetParameters(ctx, height)
}

// Implements consensus.LightClientBackend.
func (lc *lightClient) State() syncer.ReadSyncer {
	return lc.getPrimary().State()
}

// Implements consensus.LightClientBackend.
func (lc *lightClient) SubmitTxNoWait(ctx context.Context, tx *transaction.SignedTransaction) error {
	return lc.getPrimary().SubmitTxNoWait(ctx, tx)
}

// Implements consensus.LightClientBackend.
func (lc *lightClient) SubmitEvidence(ctx context.Context, evidence *consensus.Evidence) error {
	return lc.getPrimary().SubmitEvidence(ctx, evidence)
}

// Implements Client.
func (lc *lightClient) GetVerifiedLightBlock(ctx context.Context, height int64) (*tmtypes.LightBlock, error) {
	return lc.tmc.VerifyLightBlockAtHeight(ctx, height, time.Now())
}

// Implements Client.
func (lc *lightClient) GetVerifiedParameters(ctx context.Context, height int64) (*tmproto.ConsensusParams, error) {
	p, err := lc.getPrimary().GetParameters(ctx, height)
	if err != nil {
		return nil, err
	}
	if p.Height <= 0 {
		return nil, fmt.Errorf("malformed height in response: %d", p.Height)
	}

	// Decode Tendermint-specific parameters.
	var params tmproto.ConsensusParams
	if err = params.Unmarshal(p.Meta); err != nil {
		return nil, fmt.Errorf("malformed parameters: %w", err)
	}
	if err = tmtypes.ValidateConsensusParams(params); err != nil {
		return nil, fmt.Errorf("malformed parameters: %w", err)
	}

	// Fetch the header from the light client.
	l, err := lc.tmc.VerifyLightBlockAtHeight(ctx, p.Height, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to fetch header %d from light client: %w", p.Height, err)
	}

	// Verify hash.
	if localHash := tmtypes.HashConsensusParams(params); !bytes.Equal(localHash, l.ConsensusHash) {
		return nil, fmt.Errorf("mismatched parameters hash (expected: %X got: %X)",
			l.ConsensusHash,
			localHash,
		)
	}

	return &params, nil
}

func (lc *lightClient) getPrimary() consensus.LightClientBackend {
	return lc.tmc.Primary().(light.Provider).Backend()
}

// NewClient creates a new light client.
func NewClient(ctx context.Context, cfg consensus.LightClientConfig, p2pClient p2prpc.Client) (consensus.LightClient, error) {
	if numNodes := len(cfg.ConsensusNodes) + int(cfg.P2PNodes); numNodes < 2 {
		return nil, fmt.Errorf("at least two consensus grpc or p2p nodes must be configured (got %d)", numNodes)
	}

	var providers []tmlightprovider.Provider
	for _, address := range cfg.ConsensusNodes {
		p, err := grpc.NewLightClientProvider(cfg.GenesisDocument.ChainID, address)
		if err != nil {
			return nil, fmt.Errorf("failed to create grpc light client provider: %w", err)
		}
		providers = append(providers, p)
	}
	if p2pClient != nil {
		for i := 0; i < int(cfg.P2PNodes); i++ {
			p, err := p2p.NewLightClientProvider(ctx, cfg.GenesisDocument.ChainID, p2pClient)
			if err != nil {
				return nil, fmt.Errorf("failed to create p2p light client provider: %w", err)
			}
			providers = append(providers, p)
		}
	}

	tmc, err := tmlight.NewClient(
		ctx,
		cfg.GenesisDocument.ChainID,
		cfg.TrustOptions,
		providers[0],                       // Primary provider.
		providers[1:],                      // Witnesses.
		tmlightdb.New(tmdb.NewMemDB(), ""), // TODO: Make the database configurable.
		tmlight.MaxRetryAttempts(5),        // TODO: Make this configurable.
		tmlight.Logger(common.NewLogAdapter(!viper.GetBool(common.CfgLogDebug))),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create light client: %w", err)
	}

	return &lightClient{
		tmc: tmc,
	}, nil
}
