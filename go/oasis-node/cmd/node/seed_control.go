package node

import (
	"context"

	"github.com/oasisprotocol/oasis-core/go/common/version"
	control "github.com/oasisprotocol/oasis-core/go/control/api"
	upgrade "github.com/oasisprotocol/oasis-core/go/upgrade/api"
)

// Assert that the seed node implements NodeController interface.
var _ control.NodeController = (*SeedNode)(nil)

// RequestShutdown implements control.NodeController.
func (n *SeedNode) RequestShutdown(ctx context.Context, wait bool) error {
	n.Stop()
	return nil
}

// WaitReady implements control.NodeController.
func (n *SeedNode) WaitReady(ctx context.Context) error {
	return control.ErrNotImplemented
}

// IsReady implements control.NodeController.
func (n *SeedNode) IsReady(ctx context.Context) (bool, error) {
	return false, control.ErrNotImplemented
}

// WaitSync implements control.NodeController.
func (n *SeedNode) WaitSync(ctx context.Context) error {
	return control.ErrNotImplemented
}

// IsSynced implements control.NodeController.
func (n *SeedNode) IsSynced(ctx context.Context) (bool, error) {
	return false, control.ErrNotImplemented
}

// UpgradeBinary implements control.NodeController.
func (n *SeedNode) UpgradeBinary(ctx context.Context, descriptor *upgrade.Descriptor) error {
	return control.ErrNotImplemented
}

// CancelUpgrade implements control.NodeController.
func (n *SeedNode) CancelUpgrade(ctx context.Context, descriptor *upgrade.Descriptor) error {
	return control.ErrNotImplemented
}

// GetStatus implements control.NodeController.
func (n *SeedNode) GetStatus(ctx context.Context) (*control.Status, error) {
	addresses, err := n.tendermintSeed.GetAddresses()
	if err != nil {
		return nil, err
	}

	seedStatus := control.SeedStatus{
		ChainContext: n.tendermintSeed.GetChainContext(),
		NodePeers:    n.tendermintSeed.GetPeers(),
		Addresses:    addresses,
	}

	identity := control.IdentityStatus{
		Node:      n.identity.NodeSigner.Public(),
		P2P:       n.identity.P2PSigner.Public(),
		Consensus: n.identity.ConsensusSigner.Public(),
		TLS:       n.identity.GetTLSPubKeys(),
	}

	return &control.Status{
		SoftwareVersion: version.SoftwareVersion,
		Identity:        identity,
		Seed:            &seedStatus,
	}, nil
}
