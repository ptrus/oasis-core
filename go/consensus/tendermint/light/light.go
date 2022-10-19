// Package light provides a light Tendermint consensus backend implementation.
package light

import (
	tmlightprovider "github.com/tendermint/tendermint/light/provider"

	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
)

// Provider is a Tendermint light client provider implementing the LightClientBackend interface.
type Provider interface {
	tmlightprovider.Provider

	Backend() consensus.LightClientBackend
}
