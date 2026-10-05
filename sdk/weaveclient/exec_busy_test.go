package weaveclient_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Exec input against weavemoduletest.Core when the exec module's queue is
// full for a while, or the module stops: a module that reassembles its input
// with the same rules the real one's stream obeys, so a chunk lost, repeated
// or out of order fails the exec rather than passing unnoticed.

// stdinRecorder is an exec module that keeps what its one exec was sent and
// exits once the input ends.
type stdinRecorder struct {
	mu  sync.Mutex
	asm weavewire.StreamAssembler
	got []byte
}

func (r *stdinRecorder) Capability() weavewire.Capability { return weavewire.Exec }

func (r *stdinRecorder) Register(reg *weavemodule.Registrar) error {
	emit := reg.Emitter()
	reg.Handle(weavewire.KindExecStart, func(_ context.Context, payload []byte) ([]byte, error) {
		var req weavewire.ExecRequest
		_ = json.Unmarshal(payload, &req)
		return weavewire.EncodePayload(weavewire.ExecStartResponse{ExecID: req.ExecID, PID: 1})
	})
	reg.Handle(weavewire.KindExecStdin, func(ctx context.Context, payload []byte) ([]byte, error) {
		var chunk weavewire.Chunk
		_ = json.Unmarshal(payload, &chunk)
		r.mu.Lock()
		data, err := r.asm.Accept(chunk)
		r.got = append(r.got, data...)
		done := r.asm.Done()
		r.mu.Unlock()
		switch {
		case err != nil:
			return nil, emit.Emit(ctx, weavewire.KindExecExit, weavewire.ExecExit{
				ExecID: chunk.StreamID, Code: -1, Err: err.Error(),
			})
		case done:
			return nil, emit.Emit(ctx, weavewire.KindExecExit,
				weavewire.ExecExit{ExecID: chunk.StreamID})
		}
		return nil, nil
	})
	return nil
}

func (r *stdinRecorder) received() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.got)
}

// wireExec is an authenticated client and a core serving rec as the exec
// module, returning the module for a test that stops it.
func wireExec(
	t *testing.T,
	rec *stdinRecorder,
) (*weaveclient.Client, *weavemoduletest.Core, *weavemodule.Module) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	m := core.Serve(t, rec)
	go core.Run()
	ctx, cancel := context.WithCancel(context.Background())
	client := weaveclient.New(ctx, hostConn, weaveclient.Options{Log: quietLog()})
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		_ = core.Close()
	})
	if err := client.Authenticate(timeout(t), priv); err != nil {
		t.Fatal(err)
	}
	return client, core, m
}

// A module whose queue is full refuses input as busy; the input goes again,
// in order, until it is taken — the end of it too — and the process sees
// every byte once.
func TestExecInputRidesOutABusyModule(t *testing.T) {
	rec := &stdinRecorder{}
	client, core, _ := wireExec(t, rec)
	ctx := timeout(t)

	s, err := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"cat"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 3*weavewire.MaxChunkBytes+123)
	_, _ = rand.Read(want)

	core.SetBusyFor("weave.exec", 6)
	if n, err := s.Write(want[:weavewire.MaxChunkBytes+7]); err != nil ||
		n != weavewire.MaxChunkBytes+7 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if _, err := s.WriteContext(ctx, want[weavewire.MaxChunkBytes+7:]); err != nil {
		t.Fatal(err)
	}
	// The end of input is refused for a while too, and still gets there.
	core.SetBusyFor("weave.exec", 4)
	if err := s.CloseStdin(); err != nil {
		t.Fatal(err)
	}
	if code, err := s.Wait(ctx); code != 0 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	if got := rec.received(); !bytes.Equal(got, want) {
		t.Fatalf("the process got %d bytes, want the %d sent, in order", len(got), len(want))
	}
}

// A module that stops mid-stream ends the session: the input fails with
// core's reason, and so does Wait.
func TestExecInputToAModuleThatStops(t *testing.T) {
	rec := &stdinRecorder{}
	client, core, m := wireExec(t, rec)
	ctx := timeout(t)

	s, err := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"cat"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	core.SetModule(hvchannel.ModuleInfo{
		ID: m.ID(), Address: m.Address(), State: "backoff", Detail: "exited 1",
	})
	if _, err := s.Write([]byte("after")); !errors.Is(err, weaveclient.ErrModuleNotRunning) {
		t.Fatalf("Write after the module stopped = %v", err)
	}
	if err := s.CloseStdin(); !errors.Is(err, weaveclient.ErrModuleNotRunning) {
		t.Fatalf("CloseStdin after the module stopped = %v", err)
	}
	if code, err := s.Wait(ctx); code != -1 || !errors.Is(err, weaveclient.ErrModuleNotRunning) {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	if got := string(rec.received()); got != "before" {
		t.Fatalf("the process got %q", got)
	}
}
