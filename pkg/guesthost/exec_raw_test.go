package guesthost_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guesthost"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// Exec stream handling against a hand-driven guest, so it runs on every OS and
// can produce what a well-behaved guest never would.

func startExec(
	t *testing.T,
	guest *rawGuest,
	client *guesthost.Client,
	ctx context.Context,
) *guesthost.ExecSession {
	t.Helper()
	type started struct {
		s   *guesthost.ExecSession
		err error
	}
	done := make(chan started, 1)
	go func() {
		s, err := client.Exec(ctx, guestwire.ExecRequest{Argv: []string{"x"}, Stdin: true})
		done <- started{s, err}
	}()
	cmd := guest.read()
	var in guestwire.Command
	_ = json.Unmarshal(cmd.Data, &in)
	var req guestwire.ExecRequest
	_ = json.Unmarshal(in.Payload, &req)
	payload, _ := json.Marshal(guestwire.ExecStartResponse{ExecID: req.ExecID, PID: 7})
	guest.reply(cmd, guestwire.Result{Payload: payload})
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.s.ID() != req.ExecID || r.s.PID() != 7 {
		t.Fatalf("session %s/%d, want %s/7", r.s.ID(), r.s.PID(), req.ExecID)
	}
	return r.s
}

func (g *rawGuest) event(kind string, payload any) {
	g.t.Helper()
	data, _ := json.Marshal(payload)
	g.write(hvchannel.Envelope{Module: "guestweave.exec", Kind: kind, Data: data})
}

// A gap in the output is fatal to that stream: the reader gets the bytes
// before the gap and then an error, never a silently holed stream.
func TestExecStreamGapIsAnError(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := startExec(t, guest, client, ctx)

	guest.event(
		guestwire.KindExecStdout,
		guestwire.Chunk{StreamID: s.ID(), Seq: 0, Data: []byte("before")},
	)
	guest.event(
		guestwire.KindExecStdout,
		guestwire.Chunk{StreamID: s.ID(), Seq: 2, Data: []byte("after")},
	)
	// Late chunks after the stream failed are ignored.
	guest.event(
		guestwire.KindExecStdout,
		guestwire.Chunk{StreamID: s.ID(), Seq: 3, Data: []byte("late")},
	)

	out, err := io.ReadAll(s.Stdout())
	if string(out) != "before" || err == nil {
		t.Fatalf("read %q, %v; want the bytes before the gap and an error", out, err)
	}
	guest.event(guestwire.KindExecExit, guestwire.ExecExit{ExecID: s.ID(), Code: 0})
	if _, err := s.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

// A guest-side failure ends the stream with its reason and the exit with it.
func TestExecProducerFailureReachesTheCaller(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := startExec(t, guest, client, ctx)

	guest.event(
		guestwire.KindExecStderr,
		guestwire.Chunk{
			StreamID: s.ID(),
			Seq:      0,
			Data:     []byte("partial"),
			EOF:      true,
			Err:      "disk gone",
		},
	)
	// Noise: undecodable events, and events for execs nobody started.
	guest.write(
		hvchannel.Envelope{
			Module: "guestweave.exec",
			Kind:   guestwire.KindExecStdout,
			Data:   []byte("{"),
		},
	)
	guest.write(
		hvchannel.Envelope{
			Module: "guestweave.exec",
			Kind:   guestwire.KindExecExit,
			Data:   []byte("{"),
		},
	)
	guest.event(guestwire.KindExecStdout, guestwire.Chunk{StreamID: "nobody", Seq: 0})
	guest.event(guestwire.KindExecExit, guestwire.ExecExit{ExecID: "nobody"})
	guest.event(
		guestwire.KindExecExit,
		guestwire.ExecExit{
			ExecID: s.ID(),
			Code:   1,
			Signal: "KILL",
			Err:    "output exceeded the limit",
		},
	)

	out, err := io.ReadAll(s.Stderr())
	if string(out) != "partial" || err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Fatalf("stderr = %q, %v", out, err)
	}
	code, err := s.Wait(ctx)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("wait = %d, %v", code, err)
	}
	if s.ExitSignal() != "KILL" {
		t.Fatalf("signal = %q", s.ExitSignal())
	}
}

// Input larger than a chunk is split, each piece numbered, and the guest can
// reassemble it exactly.
func TestExecStdinIsChunked(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := startExec(t, guest, client, ctx)

	input := strings.Repeat("x", guestwire.MaxChunkBytes+10)
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(s, input)
		if err == nil {
			err = s.CloseStdin()
		}
		written <- err
	}()
	var asm guestwire.StreamAssembler
	var got strings.Builder
	for !asm.Done() {
		env := guest.read()
		if env.Module != "guestweave.exec" || env.Kind != guestwire.KindExecStdin {
			t.Fatalf("got %s/%s", env.Module, env.Kind)
		}
		var cmd guestwire.Command
		_ = json.Unmarshal(env.Data, &cmd)
		if cmd.ID != "" {
			t.Fatal("stdin carried a correlation id; it would be answered")
		}
		var c guestwire.Chunk
		_ = json.Unmarshal(cmd.Payload, &c)
		data, err := asm.Accept(c)
		if err != nil {
			t.Fatal(err)
		}
		got.Write(data)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if got.String() != input {
		t.Fatalf("guest reassembled %d bytes, want %d", got.Len(), len(input))
	}
}

func TestExecWaitEndsWithItsContextOrTheChannel(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := startExec(t, guest, client, ctx)

	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if code, err := s.Wait(short); code != -1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait = %d, %v", code, err)
	}

	_ = guest.conn.Close()
	if code, err := s.Wait(ctx); code != -1 || err == nil {
		t.Fatalf("wait on a dead channel = %d, %v", code, err)
	}
	if _, err := s.Write([]byte("x")); err == nil {
		t.Fatal("wrote to a dead channel")
	}
}

func TestExecStartRefusalIsAnError(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := client.Exec(ctx, guestwire.ExecRequest{Argv: []string{"x"}})
		errc <- err
	}()
	guest.reply(guest.read(), guestwire.Result{Err: "denied by policy"})
	var ge *guesthost.GuestError
	if err := <-errc; !errors.As(err, &ge) {
		t.Fatalf("err = %v", err)
	}
}

func TestPowerRefusalIsNotAccepted(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := client.Shutdown(ctx, "r")
		done <- result{ok, err}
	}()
	guest.reply(guest.read(), guestwire.Result{Err: "no privilege"})
	if r := <-done; r.ok || r.err == nil {
		t.Fatalf("shutdown = %v, %v", r.ok, r.err)
	}
}
