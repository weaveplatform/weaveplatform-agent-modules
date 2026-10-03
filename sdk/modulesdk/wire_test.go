package modulesdk

import (
	"log/slog"
	"math"
	"strconv"
	"testing"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
)

func TestWireConversions(t *testing.T) {
	if got := PeerHypervisor.wire(); got != agentv1.Peer_PEER_HYPERVISOR {
		t.Errorf("PeerHypervisor.wire() = %v", got)
	}
	if got := HealthDegraded.wire(); got != agentv1.Health_STATUS_DEGRADED {
		t.Errorf("HealthDegraded.wire() = %v", got)
	}
	if got := wireLevel(slog.LevelWarn); got != int32(slog.LevelWarn) {
		t.Errorf("wireLevel(warn) = %d", got)
	}

	// Values beyond int32 exist only where int is 64 bits.
	if strconv.IntSize < 64 {
		t.Skip("int is 32 bits: nothing can overflow")
	}
	big := math.MaxInt32 + 1
	if got := Peer(big).wire(); got != agentv1.Peer_PEER_UNSPECIFIED {
		t.Errorf("oversized Peer.wire() = %v, want unspecified", got)
	}
	if got := HealthStatus(-big - 1).wire(); got != agentv1.Health_STATUS_UNSPECIFIED {
		t.Errorf("undersized HealthStatus.wire() = %v, want unspecified", got)
	}
	if got := wireLevel(slog.Level(big)); got != math.MaxInt32 {
		t.Errorf("wireLevel(huge) = %d, want MaxInt32", got)
	}
	if got := wireLevel(slog.Level(-big - 1)); got != math.MinInt32 {
		t.Errorf("wireLevel(tiny) = %d, want MinInt32", got)
	}
}
