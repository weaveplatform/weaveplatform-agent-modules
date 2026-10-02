package weaveagent

import (
	"context"
	"errors"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
)

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", name)
		}
	}()
	fn()
}

func TestRegistrationMistakesPanic(t *testing.T) {
	d := New(&captureSender{}, nil)
	d.Handle(
		"weave.time.get",
		func(context.Context, []byte) ([]byte, error) { return nil, nil },
	)
	mustPanic(t, "a duplicate", func() {
		d.Handle(
			"weave.time.get",
			func(context.Context, []byte) ([]byte, error) { return nil, nil },
		)
	})
	mustPanic(t, "a deferred reply kind", func() {
		d.HandleDeferred("weave.time.get.result", nil)
	})
}

// A frame that is not a Command has no id to reply to; it is dropped without
// a reply rather than answered with a guess.
func TestUndecodableCommandsGetNoReply(t *testing.T) {
	cs := &captureSender{}
	d := New(cs, nil)
	replies := runOne(
		t,
		d,
		modulesdk.Message{
			Peer: modulesdk.PeerHypervisor,
			Kind: "weave.time.get",
			Data: []byte("{"),
		},
	)
	if len(replies) != 0 {
		t.Fatalf("replied %d times to an undecodable command", len(replies))
	}
}

// Deferred work on a command with no correlation id runs: there is no reply
// whose delivery it must wait for.
func TestDeferredWorkRunsForUnacknowledgedCommands(t *testing.T) {
	ran := make(chan struct{}, 1)
	d := New(&captureSender{}, nil)
	d.HandleDeferred("weave.time.set", func(context.Context, []byte) ([]byte, func(), error) {
		return nil, func() { ran <- struct{}{} }, errors.New("logged, not replied")
	})
	runOne(t, d, mustCommand(t, "weave.time.set", "", nil))
	select {
	case <-ran:
	default:
		t.Fatal("deferred work did not run")
	}
}

// A handler that returns bytes that are not JSON cannot be put in a Result;
// the dispatcher must not send a corrupt reply, nor run deferred work the host
// was never told about.
func TestAnUnencodableResponseIsNotSent(t *testing.T) {
	cs := &captureSender{}
	ran := false
	d := New(cs, nil)
	d.HandleDeferred("weave.time.get", func(context.Context, []byte) ([]byte, func(), error) {
		return []byte("{"), func() { ran = true }, nil
	})
	if replies := runOne(
		t,
		d,
		mustCommand(t, "weave.time.get", "id", nil),
	); len(replies) != 0 ||
		ran {
		t.Fatalf("replies = %d, deferred ran = %v", len(replies), ran)
	}
}

type failingSender struct{}

func (failingSender) Send(context.Context, modulesdk.Message, bool) (bool, error) {
	return false, errors.New("core went away")
}

func TestEmitReportsEncodingAndSendFailures(t *testing.T) {
	if err := NewEmitter(
		&captureSender{},
	).Emit(context.Background(), "weave.exec.stdout", func() {}); err == nil {
		t.Error("an unencodable event was sent")
	}
	if err := NewEmitter(
		failingSender{},
	).Emit(context.Background(), "weave.exec.stdout", nil); err == nil {
		t.Error("a failed send was reported as delivered")
	}
	w := NewStreamWriter(NewEmitter(failingSender{}), "weave.exec.stdout", "s")
	if n, err := w.Write([]byte("data")); err == nil || n != 0 {
		t.Errorf("write over a dead transport = %d, %v", n, err)
	}
}
