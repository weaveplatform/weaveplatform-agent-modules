package weaveclient_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Confirmed exec input against a hand-driven guest: the frames on the wire,
// and every way the wait for a busy module can end.

// registryCore makes the raw guest a core that has shown it has a registry,
// so the client confirms exec input.
func registryCore(t *testing.T, guest *rawGuest, client *weaveclient.Client) {
	t.Helper()
	guest.control(hvchannel.KindModulesChanged, "", weaveclient.ModulesSnapshot{Revision: 1})
	awaitSnapshot(t, client, 1)
}

func readChunk(t *testing.T, guest *rawGuest) weavewire.Chunk {
	t.Helper()
	env := guest.read()
	if env.Kind != weavewire.KindExecStdin {
		t.Fatalf("sent %s, want stdin", env.Kind)
	}
	var cmd weavewire.Command
	_ = json.Unmarshal(env.Data, &cmd)
	var chunk weavewire.Chunk
	_ = json.Unmarshal(cmd.Payload, &chunk)
	return chunk
}

// readFence reads the modules.list a confirmed chunk is followed by.
func readFence(t *testing.T, guest *rawGuest) string {
	t.Helper()
	env := guest.read()
	if env.Kind != hvchannel.KindModulesList || env.ID == "" {
		t.Fatalf("sent %s %q, want a modules.list fence", env.Kind, env.ID)
	}
	return env.ID
}

func (g *rawGuest) refuse(execID, reason string) {
	g.t.Helper()
	g.control(hvchannel.KindDeliveryFailed, execID, hvchannel.DeliveryFailed{
		Module: "weave.exec", Kind: weavewire.KindExecStdin, Reason: reason,
	})
}

func (g *rawGuest) answerFence(id string) {
	g.t.Helper()
	g.control(hvchannel.KindModulesListResult, id, weaveclient.ModulesSnapshot{Revision: 1})
}

// busyGuest refuses every input chunk as busy and answers every fence,
// until the channel ends, calling after once the first refusal is out. It
// reads without the test's helpers, which must not run after the test ends.
func busyGuest(guest *rawGuest, execID string, after func()) {
	refused := false
	for {
		env, err := hvchannel.ReadEnvelope(guest.r)
		if err != nil {
			return
		}
		if env.Kind != hvchannel.KindModulesList {
			continue
		}
		write := func(kind, id string, payload any) {
			data, _ := json.Marshal(payload)
			_ = hvchannel.WriteEnvelope(guest.w, hvchannel.Envelope{
				Module: hvchannel.ControlModule, Kind: kind, Data: data, ID: id,
			})
			_ = guest.w.Flush()
		}
		write(hvchannel.KindDeliveryFailed, execID, hvchannel.DeliveryFailed{
			Module: "weave.exec", Kind: weavewire.KindExecStdin, Reason: hvchannel.ReasonBusy,
		})
		write(hvchannel.KindModulesListResult, env.ID, weaveclient.ModulesSnapshot{Revision: 1})
		if !refused && after != nil {
			refused = true
			after()
		}
	}
}

// Each chunk is followed by a fence and waits for it; a busy refusal sends
// the same chunk again, with the same sequence number, and the next one
// only after it is taken.
func TestExecInputIsConfirmedChunkByChunk(t *testing.T) {
	guest, client := newRaw(t)
	ctx := timeout(t)
	registryCore(t, guest, client)
	s := startExec(t, guest, client, ctx)

	wrote := make(chan error, 1)
	go func() { _, err := s.Write([]byte("abc")); wrote <- err }()
	for attempt := range 3 {
		if c := readChunk(t, guest); c.Seq != 0 || string(c.Data) != "abc" {
			t.Fatalf("attempt %d sent %+v", attempt, c)
		}
		fence := readFence(t, guest)
		if attempt < 2 {
			guest.refuse(s.ID(), hvchannel.ReasonBusy)
		}
		guest.answerFence(fence)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}

	closed := make(chan error, 1)
	go func() { closed <- s.CloseStdin() }()
	if c := readChunk(t, guest); c.Seq != 1 || !c.EOF {
		t.Fatalf("end of input sent as %+v", c)
	}
	guest.answerFence(readFence(t, guest))
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// The wait for a busy module ends with the caller's context, the process's
// exit or the channel's end — each saying the module was busy, where that
// is why the input did not get there.
func TestExecInputWaitForABusyModuleEnds(t *testing.T) {
	t.Run("context", func(t *testing.T) {
		guest, client := newRaw(t)
		registryCore(t, guest, client)
		s := startExec(t, guest, client, timeout(t))
		go busyGuest(guest, s.ID(), nil)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := s.WriteContext(ctx, []byte("x"))
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, weaveclient.ErrModuleBusy) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("exit", func(t *testing.T) {
		guest, client := newRaw(t)
		registryCore(t, guest, client)
		s := startExec(t, guest, client, timeout(t))
		go busyGuest(guest, s.ID(), func() {
			data, _ := json.Marshal(weavewire.ExecExit{ExecID: s.ID(), Code: 2})
			_ = hvchannel.WriteEnvelope(guest.w, hvchannel.Envelope{
				Module: "weave.exec", Kind: weavewire.KindExecExit, Data: data,
			})
			_ = guest.w.Flush()
		})

		err := s.CloseStdinContext(timeout(t))
		if !errors.Is(err, weaveclient.ErrModuleBusy) || !strings.Contains(err.Error(), "ended") {
			t.Fatalf("err = %v", err)
		}
		if code, err := s.Wait(timeout(t)); code != 2 || err != nil {
			t.Fatalf("Wait = %d, %v", code, err)
		}
	})
	t.Run("channel", func(t *testing.T) {
		guest, client := newRaw(t)
		registryCore(t, guest, client)
		s := startExec(t, guest, client, timeout(t))
		go busyGuest(guest, s.ID(), func() { _ = client.Close() })

		if _, err := s.Write([]byte("x")); !errors.Is(err, weaveclient.ErrClosed) {
			t.Fatalf("err = %v", err)
		}
		if _, err := s.Write([]byte("x")); !errors.Is(err, weaveclient.ErrClosed) {
			t.Fatalf("a write on a closed channel = %v", err)
		}
	})
}

// A module that is gone ends the session while its chunk is being
// confirmed; a fence core refuses fails the input with core's reason.
func TestExecInputConfirmationFailures(t *testing.T) {
	t.Run("not running", func(t *testing.T) {
		guest, client := newRaw(t)
		ctx := timeout(t)
		registryCore(t, guest, client)
		s := startExec(t, guest, client, ctx)

		wrote := make(chan error, 1)
		go func() { _, err := s.Write([]byte("x")); wrote <- err }()
		readChunk(t, guest)
		fence := readFence(t, guest)
		guest.refuse(s.ID(), hvchannel.ReasonNotRunning)
		guest.answerFence(fence)
		if err := <-wrote; !errors.Is(err, weaveclient.ErrModuleNotRunning) {
			t.Fatalf("Write = %v", err)
		}
		if code, err := s.Wait(
			ctx,
		); code != -1 ||
			!errors.Is(err, weaveclient.ErrModuleNotRunning) {
			t.Fatalf("Wait = %d, %v", code, err)
		}
	})
	t.Run("fence refused", func(t *testing.T) {
		guest, client := newRaw(t)
		registryCore(t, guest, client)
		s := startExec(t, guest, client, timeout(t))

		wrote := make(chan error, 1)
		go func() { _, err := s.Write([]byte("x")); wrote <- err }()
		readChunk(t, guest)
		guest.control(
			hvchannel.KindAuthResult,
			readFence(t, guest),
			hvchannel.AuthResult{Reason: "no"},
		)
		if err := <-wrote; !errors.Is(err, weaveclient.ErrNotAuthenticated) {
			t.Fatalf("Write = %v", err)
		}
	})
}

// Input sent before the client knew core reports refusals goes unconfirmed.
// Once it knows, one fence goes out before the next chunk, so a refusal of
// the earlier input is not taken for the new chunk's — and stops the input,
// since the refused chunk cannot be resent in order.
func TestExecInputBecomesConfirmed(t *testing.T) {
	setup := func(t *testing.T) (*rawGuest, *weaveclient.Client, *weaveclient.ExecSession) {
		t.Helper()
		guest, client := newRaw(t)
		s := startExec(t, guest, client, timeout(t))
		// Unconfirmed: Write returns once the frame is written, no fence.
		wrote := make(chan error, 1)
		go func() { _, err := s.Write([]byte("early")); wrote <- err }()
		if c := readChunk(t, guest); c.Seq != 0 {
			t.Fatalf("sent %+v", c)
		}
		if err := <-wrote; err != nil {
			t.Fatal(err)
		}
		registryCore(t, guest, client)
		return guest, client, s
	}
	t.Run("taken", func(t *testing.T) {
		guest, _, s := setup(t)
		wrote := make(chan error, 1)
		go func() { _, err := s.Write([]byte("late")); wrote <- err }()
		guest.answerFence(readFence(t, guest))
		if c := readChunk(t, guest); c.Seq != 1 || string(c.Data) != "late" {
			t.Fatalf("sent %+v", c)
		}
		guest.answerFence(readFence(t, guest))
		if err := <-wrote; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the early input was refused", func(t *testing.T) {
		guest, _, s := setup(t)
		wrote := make(chan error, 1)
		go func() { _, err := s.Write([]byte("late")); wrote <- err }()
		fence := readFence(t, guest)
		guest.refuse(s.ID(), hvchannel.ReasonBusy)
		guest.answerFence(fence)
		err := <-wrote
		if !errors.Is(err, weaveclient.ErrModuleBusy) || !strings.Contains(err.Error(), "lost") {
			t.Fatalf("Write = %v", err)
		}
	})
	t.Run("channel closed", func(t *testing.T) {
		_, client, s := setup(t)
		_ = client.Close()
		if _, err := s.Write([]byte("late")); !errors.Is(err, weaveclient.ErrClosed) {
			t.Fatalf("Write = %v", err)
		}
	})
}

// Input straight after authenticating waits for the registry fetch that
// follows it, and is confirmed when core answers it; the wait ends with the
// caller's context or the channel.
func TestExecInputWaitsForTheRegistryFetch(t *testing.T) {
	authenticated := func(t *testing.T) (*rawGuest, *weaveclient.Client, *weaveclient.ExecSession, string) {
		t.Helper()
		guest, client := newRawWith(t, weaveclient.Options{RegistryTimeout: -1})
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		authed := make(chan error, 1)
		go func() { authed <- client.Authenticate(timeout(t), priv) }()
		guest.handshake()
		if err := <-authed; err != nil {
			t.Fatal(err)
		}
		refresh := readFence(t, guest)
		return guest, client, startExec(t, guest, client, timeout(t)), refresh
	}
	t.Run("answered", func(t *testing.T) {
		guest, _, s, refresh := authenticated(t)
		wrote := make(chan error, 1)
		go func() { _, err := s.Write([]byte("x")); wrote <- err }()
		guest.answerFence(refresh)
		readChunk(t, guest)
		guest.answerFence(readFence(t, guest))
		if err := <-wrote; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("context", func(t *testing.T) {
		_, _, s, _ := authenticated(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.WriteContext(ctx, []byte("x")); !errors.Is(err, context.Canceled) {
			t.Fatalf("Write = %v", err)
		}
	})
	t.Run("channel", func(t *testing.T) {
		_, client, s, _ := authenticated(t)
		wrote := make(chan error, 1)
		go func() { _, err := s.Write([]byte("x")); wrote <- err }()
		time.Sleep(10 * time.Millisecond)
		_ = client.Close()
		if err := <-wrote; !errors.Is(err, weaveclient.ErrClosed) {
			t.Fatalf("Write = %v", err)
		}
	})
}
