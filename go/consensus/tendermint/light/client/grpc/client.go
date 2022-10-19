package grpc

import (
	"context"
	"errors"
	"fmt"

	tmlightprovider "github.com/tendermint/tendermint/light/provider"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	tmtypes "github.com/tendermint/tendermint/types"
	"google.golang.org/grpc"

	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	cmnGrpc "github.com/oasisprotocol/oasis-core/go/common/grpc"
	"github.com/oasisprotocol/oasis-core/go/common/identity"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
)

// lightClientProvider implements Tendermint's light client provider interface using the Oasis Core
// light client API.
type lightClientProvider struct {
	chainID string
	client  consensus.LightClientBackend
}

// Implements tmlightprovider.Provider.
func (lp *lightClientProvider) ChainID() string {
	return lp.chainID
}

// Implements tmlightprovider.Provider.
func (lp *lightClientProvider) LightBlock(ctx context.Context, height int64) (*tmtypes.LightBlock, error) {
	lb, err := lp.client.GetLightBlock(ctx, height)
	switch {
	case err == nil:
	case errors.Is(err, consensus.ErrVersionNotFound):
		return nil, tmlightprovider.ErrLightBlockNotFound
	default:
		return nil, tmlightprovider.ErrNoResponse
	}

	// Decode Tendermint-specific light block.
	var protoLb tmproto.LightBlock
	if err = protoLb.Unmarshal(lb.Meta); err != nil {
		return nil, tmlightprovider.ErrBadLightBlock{Reason: err}
	}
	tlb, err := tmtypes.LightBlockFromProto(&protoLb)
	if err != nil {
		return nil, tmlightprovider.ErrBadLightBlock{Reason: err}
	}
	if err = tlb.ValidateBasic(lp.chainID); err != nil {
		return nil, tmlightprovider.ErrBadLightBlock{Reason: err}
	}

	return tlb, nil
}

// Implements tmlightprovider.Provider.
func (lp *lightClientProvider) ReportEvidence(ctx context.Context, ev tmtypes.Evidence) error {
	proto, err := tmtypes.EvidenceToProto(ev)
	if err != nil {
		return fmt.Errorf("failed to convert evidence: %w", err)
	}
	meta, err := proto.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal evidence: %w", err)
	}

	return lp.client.SubmitEvidence(ctx, &consensus.Evidence{Meta: meta})
}

// newLightClientProvider creates a new provider for the Tendermint's light client.
//
// The provided chain ID must be the Tendermint chain ID.
func NewLightClientProvider(
	chainID string,
	address node.TLSAddress,
) (tmlightprovider.Provider, error) {
	// Create TLS credentials.
	opts := cmnGrpc.ClientOptions{
		CommonName: identity.CommonName,
		ServerPubKeys: map[signature.PublicKey]bool{
			address.PubKey: true,
		},
	}
	creds, err := cmnGrpc.NewClientCreds(&opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create TLS client credentials: %w", err)
	}

	conn, err := cmnGrpc.Dial(address.Address.String(), grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("failed to dial public consensus service endpoint %s: %w", address, err)
	}

	return &lightClientProvider{
		chainID: chainID,
		client:  consensus.NewConsensusLightClient(conn),
	}, nil
}
