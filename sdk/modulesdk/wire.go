package modulesdk

import (
	"log/slog"
	"math"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
)

// The public types are Go ints so a module never handles a protobuf enum; the
// wire enums are int32. Every narrowing goes through toInt32, so a value that
// does not fit becomes the protocol's "unspecified" rather than wrapping
// around into some other, valid-looking value.

// toInt32 narrows v, reporting false when it does not fit.
func toInt32(v int) (int32, bool) {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, false
	}
	return int32(v), true
}

// wire is p as the protocol's Peer enum. Values core does not know pass
// through for core to refuse; only one that cannot be represented is sent as
// PEER_UNSPECIFIED.
func (p Peer) wire() agentv1.Peer {
	if n, ok := toInt32(int(p)); ok {
		return agentv1.Peer(n)
	}
	return agentv1.Peer_PEER_UNSPECIFIED
}

// wire is s as the protocol's Health_Status enum, on the same terms as
// Peer.wire.
func (s HealthStatus) wire() agentv1.Health_Status {
	if n, ok := toInt32(int(s)); ok {
		return agentv1.Health_Status(n)
	}
	return agentv1.Health_STATUS_UNSPECIFIED
}

// wireLevel is l as LogRecord's int32 level. slog levels are small integers
// in practice; one beyond int32 is clamped, which keeps its severity order.
func wireLevel(l slog.Level) int32 {
	if n, ok := toInt32(int(l)); ok {
		return n
	}
	if l < 0 {
		return math.MinInt32
	}
	return math.MaxInt32
}
