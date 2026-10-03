package weavepower_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavepower"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
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

func (p *scriptedPower) action(op string) (weavepower.Action, error) {
	if p.err != nil {
		return weavepower.Action{}, p.err
	}
	return weavepower.Action{
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

func (p *scriptedPower) Shutdown(context.Context, string) (weavepower.Action, error) {
	return p.action("shutdown")
}

func (p *scriptedPower) Restart(context.Context, string) (weavepower.Action, error) {
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
	if err := weavemodule.CheckParity(weavepower.NewService(&scriptedPower{})); err != nil {
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
	h := weavemoduletest.Start(t, weavepower.NewService(power), func(host *weavemoduletest.Host) {
		host.T.OnSend = func(msg modulesdk.Message) error { note("reply:" + msg.Kind); return nil }
	})

	for _, tc := range []struct{ kind, op string }{
		{weavewire.KindPowerShutdown, "shutdown"},
		{weavewire.KindPowerRestart, "restart"},
	} {
		var resp weavewire.PowerResponse
		h.Decode(tc.kind, weavewire.PowerRequest{Reason: "test"}, &resp)
		if !resp.Accepted || resp.Command != "fake "+tc.op {
			t.Fatalf("%s response = %+v", tc.op, resp)
		}
		waitFor(t, func() bool { return slices.Contains(power.didRun(), tc.op) })

		mu.Lock()
		replyAt := slices.Index(events, "reply:"+weavewire.ResultKind(tc.kind))
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
	h := weavemoduletest.Start(t, weavepower.NewService(power), func(host *weavemoduletest.Host) {
		host.T.OnSend = func(modulesdk.Message) error { return errors.New("channel gone") }
	})
	data, err := weavewire.EncodeCommand("p1", weavewire.PowerRequest{})
	if err != nil {
		t.Fatal(err)
	}
	h.T.Deliver(
		modulesdk.Message{
			Peer: modulesdk.PeerHypervisor,
			Kind: weavewire.KindPowerShutdown,
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
	h := weavemoduletest.Start(t, weavepower.NewService(power))

	res := h.Call(weavewire.KindPowerShutdown, weavewire.PowerRequest{})
	if res.Err == "" {
		t.Fatal("a backend failure was reported as success")
	}
	if len(power.didRun()) != 0 {
		t.Fatal("the action ran despite the backend refusing")
	}
}

func TestPowerRejectsAMalformedRequest(t *testing.T) {
	h := weavemoduletest.Start(t, weavepower.NewService(&scriptedPower{}))
	res := h.Call(weavewire.KindPowerRestart, "not an object")
	if res.Err == "" {
		t.Fatal("a malformed request was accepted")
	}
}

// An action that fails after the reply has nobody left to tell; it must be
// logged, not panic, and not be retried.
func TestPowerActionFailureAfterTheReplyIsContained(t *testing.T) {
	power := &scriptedPower{runErr: errors.New("shutdown(8) not found")}
	h := weavemoduletest.Start(t, weavepower.NewService(power))
	var resp weavewire.PowerResponse
	h.Decode(weavewire.KindPowerShutdown, nil, &resp)
	waitFor(t, func() bool { return len(power.didRun()) == 1 })
	if !resp.Accepted {
		t.Fatal("not accepted")
	}
}
