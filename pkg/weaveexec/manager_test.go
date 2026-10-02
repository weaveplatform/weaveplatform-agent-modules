package weaveexec

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Portable: no process is started, so these run on every OS.

type nopEmitter struct{}

func (nopEmitter) Emit(context.Context, string, any) error { return nil }

func TestStartRefusesIncompleteRequests(t *testing.T) {
	m := New(nil, nopEmitter{}, nil, nil)
	if _, err := m.Start(context.Background(), weavewire.ExecRequest{ExecID: "e"}); err == nil {
		t.Error("an exec with no program started")
	}
	if _, err := m.Start(
		context.Background(),
		weavewire.ExecRequest{Argv: []string{"x"}},
	); err == nil {
		t.Error("an exec with no id started")
	}
}

func TestOperationsOnUnknownExecsFail(t *testing.T) {
	m := New(nil, nopEmitter{}, nil, nil)
	if err := m.Stdin(weavewire.Chunk{StreamID: "ghost"}); err == nil {
		t.Error("stdin to an unknown exec")
	}
	if err := m.Resize(weavewire.ExecResizeRequest{ExecID: "ghost"}); err == nil {
		t.Error("resize of an unknown exec")
	}
	if err := m.Signal(weavewire.ExecSignalRequest{ExecID: "ghost"}); err == nil {
		t.Error("signal to an unknown exec")
	}
}

// A terminal hangup is the normal end of an interactive session, however the
// OS wraps it; anything else is a real read failure.
func TestTerminalHangupRecognition(t *testing.T) {
	if !isTerminalHangup(&os.PathError{Op: "read", Path: "/dev/ptmx", Err: errTerminalHangup}) {
		t.Error("a wrapped hangup was not recognised")
	}
	if !isTerminalHangup(errTerminalHangup) {
		t.Error("a bare hangup was not recognised")
	}
	if isTerminalHangup(io.ErrUnexpectedEOF) {
		t.Error("an ordinary error was taken for a hangup")
	}
}

// failingReader stands in for a stream that breaks mid-exec.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("pipe broke") }

type brokenProcess struct {
	killed chan struct{}
	stdin  io.WriteCloser
}

func (p *brokenProcess) Stdout() io.Reader     { return failingReader{} }
func (p *brokenProcess) Stderr() io.Reader     { return nil }
func (p *brokenProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *brokenProcess) PID() int              { return 1 }
func (p *brokenProcess) Resize(uint16, uint16) error {
	return nil
}

func (p *brokenProcess) Signal(string) error {
	close(p.killed)
	return errors.New("already gone")
}

func (p *brokenProcess) Wait() (int, string, error) {
	<-p.killed
	return -1, "", nil
}
func (p *brokenProcess) Close() error { return nil }

type brokenStarter struct{ p *brokenProcess }

func (s brokenStarter) Start(context.Context, weavewire.ExecRequest) (Process, error) {
	return s.p, nil
}

type recordingEmitter struct{ exits chan weavewire.ExecExit }

func (r recordingEmitter) Emit(_ context.Context, kind string, payload any) error {
	if exit, ok := payload.(weavewire.ExecExit); ok && kind == weavewire.KindExecExit {
		r.exits <- exit
	}
	return nil
}

// A stream that fails must kill the process — otherwise it blocks writing to a
// pipe nobody reads and never exits — and the failure must reach the exit.
func TestAStreamFailureKillsTheProcessAndIsReported(t *testing.T) {
	p := &brokenProcess{killed: make(chan struct{})}
	rec := recordingEmitter{exits: make(chan weavewire.ExecExit, 1)}
	m := New(brokenStarter{p}, rec, nil, nil)
	if _, err := m.Start(
		context.Background(),
		weavewire.ExecRequest{ExecID: "b", Argv: []string{"x"}},
	); err != nil {
		t.Fatal(err)
	}
	exit := <-rec.exits
	if !strings.Contains(exit.Err, "pipe broke") {
		t.Fatalf("exit = %+v", exit)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("process stopped reading") }
func (errWriter) Close() error              { return nil }

func TestStdinWriteFailureIsReturned(t *testing.T) {
	p := &brokenProcess{killed: make(chan struct{}), stdin: errWriter{}}
	m := New(brokenStarter{p}, recordingEmitter{exits: make(chan weavewire.ExecExit, 1)}, nil, nil)
	if _, err := m.Start(
		context.Background(),
		weavewire.ExecRequest{ExecID: "w", Argv: []string{"x"}, Stdin: true},
	); err != nil {
		t.Fatal(err)
	}
	if err := m.Stdin(weavewire.Chunk{StreamID: "w", Data: []byte("x")}); err == nil {
		t.Fatal("a failed stdin write was reported as delivered")
	}
}
