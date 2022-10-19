package light

import (
	"context"

	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	"github.com/oasisprotocol/oasis-core/go/worker/common/p2p/rpc"
)

type service struct {
	light  consensus.LightClientBackend
	logger *logging.Logger
}

func (s *service) HandleRequest(ctx context.Context, method string, body cbor.RawMessage) (interface{}, error) {
	s.logger.Debug("handling request", "method", method)
	switch method {
	case MethodGetLightBlock:
		var rq int64
		if err := cbor.Unmarshal(body, &rq); err != nil {
			return nil, rpc.ErrBadRequest
		}

		s.logger.Debug("handling get light block", "rq", rq)

		lb, err := s.light.GetLightBlock(ctx, rq)
		if err != nil {
			return nil, err
		}
		return lb, nil

	default:
		return nil, rpc.ErrMethodNotSupported
	}
}

// NewServer creates a new light block sync protocol server.
func NewServer(light consensus.LightClientBackend) rpc.Server {
	return rpc.NewServer(common.Namespace{}, LightProtocolID, LightProtocolVersion, &service{light, logging.GetLogger("p2p/server/light")})
}
