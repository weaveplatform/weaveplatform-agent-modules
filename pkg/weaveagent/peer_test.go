package weaveagent

import (
	"context"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
)

func TestPeerFromDefaultsToTheHostChannel(t *testing.T) {
	if got := PeerFrom(context.Background()); got != modulesdk.PeerHypervisor {
		t.Fatalf("no peer recorded: %v, want the host channel", got)
	}
	if got := PeerFrom(WithPeer(context.Background(), 0)); got != modulesdk.PeerHypervisor {
		t.Fatalf("unspecified peer: %v, want the host channel", got)
	}
	if got := PeerFrom(WithPeer(context.Background(), modulesdk.PeerGateWeave)); got != modulesdk.PeerGateWeave {
		t.Fatalf("recorded GateWeave: %v", got)
	}
}

// A request from the platform is answered to the platform, and so is every
// event it causes: a cloud VM has no host channel to send them down.
func TestRepliesAndEventsGoBackToTheAskingPeer(t *testing.T) {
	cs := &captureSender{}
	d := New(cs, nil)
	emit := d.Emitter()
	d.Handle("weave.echo", func(ctx context.Context, payload []byte) ([]byte, error) {
		if err := emit.Emit(ctx, "weave.echo.event", map[string]string{"x": "y"}); err != nil {
			t.Error(err)
		}
		return payload, nil
	})
	cmd := mustCommand(t, "weave.echo", "abc", nil)
	cmd.Peer = modulesdk.PeerGateWeave
	sent := runOne(t, d, cmd)
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want an event and a reply", len(sent))
	}
	for _, m := range sent {
		if m.Peer != modulesdk.PeerGateWeave {
			t.Errorf("%s went to %v, want GateWeave", m.Kind, m.Peer)
		}
	}
}
