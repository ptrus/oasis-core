package light

import (
	"time"

	"github.com/oasisprotocol/oasis-core/go/common/version"
)

// LightProtocolID is a unique protocol identifier for the light blocks sync protocol.
const LightProtocolID = "light"

// LightProtocolVersion is the supported version of the light blocks sync protocol.
var LightProtocolVersion = version.Version{Major: 1, Minor: 0, Patch: 0}

// Constants related to the GetLightBlock method.
const (
	MethodGetLightBlock          = "GetLightBlock"
	MaxGetLightBlockResponseTime = 3 * time.Second
)
