package guestpower_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule/guestmoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestpower"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// scriptedPower records what was asked of it and whether the action ran.
type scriptedPower struct {
	mu     sync.Mutex
	ran    []string
	err    error
	runErr error
	// onRun observes the ordering: it is called when the power action fires.
	onRun func(op string)
}

func (p *scriptedPower) action(op string) (guestpower.Action, error) {
	if p.err != nil {
		return guestpower.Action{}, p.err
	}
	return guestpower.Action{
		Command: "fake " + op,
		Run: func() error {
			p.mu.Lock()
			p.ran = append(p.ran, op)
			p.mu.Unlock()
			if p.onRun != nil {
				p.onRun(op)
			}
			return p.runErr
		},
	}, nil
}

func (p *scriptedPower) Shutdown(context.Context, string) (guestpower.Action, error) {
	return p.action("shutdown")
}

func (p *scriptedPower) Restart(context.Context, string) (guestpower.Action, error) {
	return p.action("restart")
}

func (p *scriptedPower) didRun() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ran)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestServesExactlyThePowerContract(t *testing.T) {
	if err := guestmodule.CheckParity(guestpower.NewService(&scriptedPower{})); err != nil {
		t.Fatal(err)
	}
}

// THE contract: the acknowledgement must be on the wire before the command that
// kills the OS runs. Reversed, the shutdown terminates the agent mid-reply and
// the host sees a dead channel instead of an answer — at which point it falls
// back to cutting the VM off mid-write, which is the exact outcome the graceful
// path exists to prevent.
func TestPowerRepliesBeforeItActs(t *testing.T) {
	var mu sync.Mutex
	var events []string
	note := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	power := &scriptedPower{onRun: func(op string) { note("ran:" + op) }}
	h := guestmoduletest.Start(t, guestpower.NewService(power), func(host *guestmoduletest.Host) {
		host.T.OnSend = func(msg modulesdk.Message) error { note("reply:" + msg.Kind); return nil }
	})

	for _, tc := range []struct{ kind, op string }{
		{guestwire.KindPowerShutdown, "shutdown"},
		{guestwire.KindPowerRestart, "restart"},
	} {
		var resp guestwire.PowerResponse
		h.Decode(tc.kind, guestwire.PowerRequest{Reason: "test"}, &resp)
		if !resp.Accepted || resp.Command != "fake "+tc.op {
			t.Fatalf("%s response = %+v", tc.op, resp)
		}
		waitFor(t, func() bool { return slices.Contains(power.didRun(), tc.op) })

		mu.Lock()
		replyAt := slices.Index(events, "reply:"+guestwire.ResultKind(tc.kind))
		ranAt := slices.Index(events, "ran:"+tc.op)
		mu.Unlock()
		if replyAt < 0 || ranAt < 0 || ranAt < replyAt {
			t.Fatalf("the guest acted before acknowledging %s: events = %v", tc.op, events)
		}
	}
}

// If the reply could not be sent, the host does not know its request was
// accepted. Powering off anyway takes a VM down while its owner believes the
// request failed — so the deferred action must be abandoned.
func TestPowerDoesNotActWhenTheReplyFails(t *testing.T) {
	power := &scriptedPower{}
	h := guestmoduletest.Start(t, guestpower.NewService(power), func(host *guestmoduletest.Host) {
		host.T.OnSend = func(modulesdk.Message) error { return errors.New("channel gone") }
	})
	data, err := guestwire.EncodeCommand("p1", guestwire.PowerRequest{})
	if err != nil {
		t.Fatal(err)
	}
	h.T.Deliver(
		modulesdk.Message{
			Peer: modulesdk.PeerHypervisor,
			Kind: guestwire.KindPowerShutdown,
			Data: data,
		},
	)

	// Give the dispatcher time to do the wrong thing if it is going to.
	time.Sleep(250 * time.Millisecond)
	if ran := power.didRun(); len(ran) != 0 {
		t.Fatalf("the guest powered off despite the reply failing: ran %v", ran)
	}
}

// A backend that cannot resolve a power action must fail in the REPLY, while
// there is still a channel to report on.
func TestPowerBackendErrorReachesTheHost(t *testing.T) {
	power := &scriptedPower{err: errors.New("no shutdown privilege")}
	h := guestmoduletest.Start(t, guestpower.NewService(power))

	res := h.Call(guestwire.KindPowerShutdown, guestwire.PowerRequest{})
	if res.Err == "" {
		t.Fatal("a backend failure was reported as success")
	}
	if len(power.didRun()) != 0 {
		t.Fatal("the action ran despite the backend refusing")
	}
}

func TestPowerRejectsAMalformedRequest(t *testing.T) {
	h := guestmoduletest.Start(t, guestpower.NewService(&scriptedPower{}))
	res := h.Call(guestwire.KindPowerRestart, "not an object")
	if res.Err == "" {
		t.Fatal("a malformed request was accepted")
	}
}

// An action that fails after the reply has nobody left to tell; it must be
// logged, not panic, and not be retried.
func TestPowerActionFailureAfterTheReplyIsContained(t *testing.T) {
	power := &scriptedPower{runErr: errors.New("shutdown(8) not found")}
	h := guestmoduletest.Start(t, guestpower.NewService(power))
	var resp guestwire.PowerResponse
	h.Decode(guestwire.KindPowerShutdown, nil, &resp)
	waitFor(t, func() bool { return len(power.didRun()) == 1 })
	if !resp.Accepted {
		t.Fatal("not accepted")
	}
}
