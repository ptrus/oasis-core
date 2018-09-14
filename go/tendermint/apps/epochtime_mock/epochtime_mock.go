// Package epochtimemock implements the mock epochtime application.
package epochtimemock

import (
	"encoding/hex"

	"github.com/pkg/errors"
	"github.com/tendermint/iavl"
	"github.com/tendermint/tendermint/abci/types"

	"github.com/oasislabs/ekiden/go/common/cbor"
	"github.com/oasislabs/ekiden/go/common/logging"
	epochtime "github.com/oasislabs/ekiden/go/epochtime/api"
	"github.com/oasislabs/ekiden/go/tendermint/abci"
	"github.com/oasislabs/ekiden/go/tendermint/api"
)

var _ abci.Application = (*epochTimeMockApplication)(nil)

type epochTimeMockApplication struct {
	logger *logging.Logger
	state  *abci.ApplicationState
}

func (app *epochTimeMockApplication) Name() string {
	return api.EpochTimeMockAppName
}

func (app *epochTimeMockApplication) TransactionTag() byte {
	return api.EpochTimeMockTransactionTag
}

func (app *epochTimeMockApplication) Blessed() bool {
	return false
}

func (app *epochTimeMockApplication) OnRegister(state *abci.ApplicationState, queryRouter abci.QueryRouter) {
	app.state = state

	// Register query handlers.
	queryRouter.AddRoute(api.QueryEpochTimeMockGetEpoch, &api.QueryGetEpoch{}, app.queryGetEpoch)
}

func (app *epochTimeMockApplication) OnCleanup() {
}

func (app *epochTimeMockApplication) SetOption(request types.RequestSetOption) types.ResponseSetOption {
	return types.ResponseSetOption{}
}

func (app *epochTimeMockApplication) GetState(height int64) (interface{}, error) {
	return NewImmutableState(app.state, height)
}

func (app *epochTimeMockApplication) queryGetEpoch(s interface{}, r interface{}) ([]byte, error) {
	state := s.(*ImmutableState)

	epoch, height, err := state.GetEpoch()
	if err != nil {
		return nil, err
	}

	response := api.QueryGetEpochResponse{
		Epoch:  epoch,
		Height: height,
	}
	return cbor.Marshal(response), nil
}

func (app *epochTimeMockApplication) CheckTx(ctx *abci.Context, tx []byte) error {
	request := &api.TxEpochTimeMock{}
	if err := cbor.Unmarshal(tx, request); err != nil {
		app.logger.Error("CheckTx: failed to unmarshal",
			"tx", hex.EncodeToString(tx),
		)
		return errors.Wrap(err, "epochtime_mock: failed to unmarshal")
	}

	if err := app.executeTx(ctx, app.state.CheckTxTree(), request); err != nil {
		return err
	}

	return nil
}

func (app *epochTimeMockApplication) ForeignCheckTx(ctx *abci.Context, other abci.Application, tx []byte) error {
	return nil
}

func (app *epochTimeMockApplication) InitChain(request types.RequestInitChain) types.ResponseInitChain {
	return types.ResponseInitChain{}
}

func (app *epochTimeMockApplication) BeginBlock(ctx *abci.Context, request types.RequestBeginBlock) {
}

func (app *epochTimeMockApplication) DeliverTx(ctx *abci.Context, tx []byte) error {
	request := &api.TxEpochTimeMock{}
	if err := cbor.Unmarshal(tx, request); err != nil {
		app.logger.Error("DeliverTx: failed to unmarshal",
			"tx", hex.EncodeToString(tx),
		)
		return errors.Wrap(err, "epochtime_mock: failed to unmarshal")
	}

	return app.executeTx(ctx, app.state.DeliverTxTree(), request)
}

func (app *epochTimeMockApplication) ForeignDeliverTx(ctx *abci.Context, other abci.Application, tx []byte) error {
	return nil
}

func (app *epochTimeMockApplication) EndBlock(request types.RequestEndBlock) types.ResponseEndBlock {
	return types.ResponseEndBlock{}
}

func (app *epochTimeMockApplication) FireTimer(ctx *abci.Context, timer *abci.Timer) {
}

func (app *epochTimeMockApplication) executeTx(
	ctx *abci.Context,
	tree *iavl.MutableTree,
	tx *api.TxEpochTimeMock,
) error {
	state := NewMutableState(tree)

	if tx.TxSetEpoch != nil {
		return app.setEpoch(ctx, state, tx.TxSetEpoch.Epoch)
	}
	return errors.New("epochtime_mock: invalid argument")
}

func (app *epochTimeMockApplication) setEpoch(
	ctx *abci.Context,
	state *MutableState,
	epoch epochtime.EpochTime,
) error {
	app.logger.Info("setting new epoch",
		"epoch", epoch,
	)

	state.SetEpoch(epoch, app.state.BlockHeight())

	ctx.EmitTag(api.TagEpochTimeMockEpoch, cbor.Marshal(epoch))

	return nil
}

// New constructs a new mock epochtime application instance.
func New() abci.Application {
	return &epochTimeMockApplication{
		logger: logging.GetLogger("tendermint/epochtime_mock"),
	}
}