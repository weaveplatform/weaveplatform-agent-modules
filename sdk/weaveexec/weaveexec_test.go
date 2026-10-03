//go:build unix

// These drive real Unix shell commands through UnixStarter. The Windows
// starter is a different implementation with its own tests in
// *_windows_test.go.

package weaveexec

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveagent"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// collector captures the events a Manager emits and reassembles the streams,
// which is exactly what the host does — so a stream this rejects would have
// failed on the host too.
type collector struct {
	mu     sync.Mutex
	stdout map[string]*strings.Builder
	stderr map[string]*strings.Builder
	asmOut map[string]*weavewire.StreamAssembler
	asmErr map[string]*weavewire.StreamAssembler
	exits  map[string]weavewire.ExecExit
	errs   []error
	done   chan weavewire.ExecExit
}

func newCollector() *collector {
	return &collector{
		stdout: map[string]*strings.Builder{},
		stderr: map[string]*strings.Builder{},
		asmOut: map[string]*weavewire.StreamAssembler{},
		asmErr: map[string]*weavewire.StreamAssembler{},
		exits:  map[string]weavewire.ExecExit{},
		done:   make(chan weavewire.ExecExit, 8),
	}
}

func (c *collector) Send(ctx context.Context, msg modulesdk.Message, _ bool) (bool, error) {
	// The real transport is a gRPC call to core, which refuses a cancelled
	// context before anything reaches the wire. A double that ignored the
	// context made every send look delivered — including one the guest never
	// actually transmitted, which is how the exec exit event went missing in
	// production with the whole suite green. Honour it here, and the suite
	// covers the constraint production imposes.
	if err := ctx.Err(); err != nil {
		return false, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	switch msg.Kind {
	case weavewire.KindExecStdout, weavewire.KindExecStderr:
		var chunk weavewire.Chunk
		if err := json.Unmarshal(msg.Data, &chunk); err != nil {
			c.errs = append(c.errs, err)
			return true, nil
		}
		sinks, asms := c.stdout, c.asmOut
		if msg.Kind == weavewire.KindExecStderr {
			sinks, asms = c.stderr, c.asmErr
		}
		if asms[chunk.StreamID] == nil {
			asms[chunk.StreamID] = &weavewire.StreamAssembler{}
			sinks[chunk.StreamID] = &strings.Builder{}
		}
		data, err := asms[chunk.StreamID].Accept(chunk)
		if err != nil {
			c.errs = append(c.errs, err)
			return true, nil
		}
		sinks[chunk.StreamID].Write(data)
	case weavewire.KindExecExit:
		var exit weavewire.ExecExit
		if err := json.Unmarshal(msg.Data, &exit); err != nil {
			c.errs = append(c.errs, err)
			return true, nil
		}
		c.exits[exit.ExecID] = exit
		c.done <- exit
	}
	return true, nil
}

func (c *collector) out(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.stdout[id]; b != nil {
		return b.String()
	}
	return ""
}

func (c *collector) errOut(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.stderr[id]; b != nil {
		return b.String()
	}
	return ""
}

func (c *collector) streamErrors() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.errs...)
}

func (c *collector) waitExit(t *testing.T) weavewire.ExecExit {
	t.Helper()
	select {
	case exit := <-c.done:
		return exit
	case <-time.After(20 * time.Second):
		t.Fatal("no exit event")
		return weavewire.ExecExit{}
	}
}

func newManager(t *testing.T, guard Guard) (*Manager, *collector) {
	t.Helper()
	c := newCollector()
	return New(UnixStarter{}, weaveagent.NewEmitter(c), guard, nil), c
}

func TestExecStreamsOutputAndReportsExit(t *testing.T) {
	m, c := newManager(t, nil)
	req := weavewire.ExecRequest{
		ExecID: "e1",
		Argv:   []string{"sh", "-c", "echo out; echo err 1>&2; exit 3"},
	}

	started, err := m.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if started.PID == 0 {
		t.Error("no pid reported")
	}

	exit := c.waitExit(t)
	if exit.Code != 3 {
		t.Errorf("exit code = %d, want 3", exit.Code)
	}
	if exit.Err != "" {
		t.Errorf("unexpected exit error: %s", exit.Err)
	}
	if got := strings.TrimSpace(c.out("e1")); got != "out" {
		t.Errorf("stdout = %q, want %q", got, "out")
	}
	if got := strings.TrimSpace(c.errOut("e1")); got != "err" {
		t.Errorf("stderr = %q, want %q", got, "err")
	}
	if errs := c.streamErrors(); len(errs) != 0 {
		t.Errorf("the streams did not reassemble cleanly: %v", errs)
	}
}

// Output larger than one chunk must reassemble byte-for-byte. This is the
// property the whole chunked design rests on, and an off-by-one at the chunk
// boundary would corrupt every large output rather than failing loudly.
func TestExecOutputLargerThanAChunkReassembles(t *testing.T) {
	m, c := newManager(t, nil)
	const lines = 4000 // comfortably more than 32 KiB
	req := weavewire.ExecRequest{
		ExecID: "big",
		Argv: []string{
			"sh",
			"-c",
			"i=0; while [ $i -lt " + itoa(lines) + " ]; do echo 0123456789ABCDEF; i=$((i+1)); done",
		},
	}
	if _, err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	exit := c.waitExit(t)
	if exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}

	want := strings.Repeat("0123456789ABCDEF\n", lines)
	if got := c.out("big"); got != want {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(want))
	}
	if errs := c.streamErrors(); len(errs) != 0 {
		t.Fatalf("stream errors: %v", errs)
	}
}

// stdin must reach the process, and closing it must let a command that reads to
// EOF finish — without that, `cat` and every script-fed shell hang forever.
func TestExecStdinReachesTheProcessAndEOFEndsIt(t *testing.T) {
	m, c := newManager(t, nil)
	req := weavewire.ExecRequest{ExecID: "in", Argv: []string{"cat"}, Stdin: true}
	if _, err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if err := m.Stdin(weavewire.Chunk{StreamID: "in", Seq: 0, Data: []byte("hello ")}); err != nil {
		t.Fatal(err)
	}
	if err := m.Stdin(weavewire.Chunk{StreamID: "in", Seq: 1, Data: []byte("stdin")}); err != nil {
		t.Fatal(err)
	}
	if err := m.Stdin(weavewire.Chunk{StreamID: "in", Seq: 2, EOF: true}); err != nil {
		t.Fatal(err)
	}

	exit := c.waitExit(t)
	if exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	if got := c.out("in"); got != "hello stdin" {
		t.Fatalf("stdout = %q", got)
	}
}

// The exit event must arrive AFTER the output it explains. A host told the
// process finished is entitled to stop reading, so output still in flight would
// be lost — and it is usually the message that explains the exit code.
func TestExitArrivesAfterOutputHasDrained(t *testing.T) {
	m, c := newManager(t, nil)
	req := weavewire.ExecRequest{
		ExecID: "order",
		Argv: []string{
			"sh",
			"-c",
			"i=0; while [ $i -lt 500 ]; do echo line-$i; i=$((i+1)); done; exit 7",
		},
	}
	if _, err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	exit := c.waitExit(t)
	if exit.Code != 7 {
		t.Fatalf("exit code = %d", exit.Code)
	}
	// Everything must already be collected at the moment the exit was seen.
	if got := strings.Count(c.out("order"), "\n"); got != 500 {
		t.Fatalf("%d lines present when the exit arrived, want 500", got)
	}
}

// A signal must reach the process, and the exit must name it — so a caller can
// distinguish "I cancelled this" from "it failed on its own".
func TestSignalTerminatesAndIsReported(t *testing.T) {
	m, c := newManager(t, nil)
	req := weavewire.ExecRequest{ExecID: "sig", Argv: []string{"sleep", "60"}}
	if _, err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// Give the process a moment to exist before signalling it.
	time.Sleep(100 * time.Millisecond)
	if err := m.Signal(
		weavewire.ExecSignalRequest{ExecID: "sig", Signal: weavewire.SignalKill},
	); err != nil {
		t.Fatal(err)
	}

	exit := c.waitExit(t)
	if exit.Signal == "" {
		t.Fatalf("exit did not report a signal: %+v", exit)
	}
	if exit.Code == 0 {
		t.Fatalf("a killed process reported success: %+v", exit)
	}
}

// A refused exec must not start anything, and the refusal must be the error the
// host receives.
func TestGuardRefusalPreventsTheProcess(t *testing.T) {
	guard := &stubGuard{refuse: errors.New("not allowed here")}
	m, _ := newManager(t, guard)

	_, err := m.Start(context.Background(), weavewire.ExecRequest{
		ExecID: "no", Argv: []string{"sh", "-c", "echo should-not-run"},
	})
	if err == nil {
		t.Fatal("a refused exec was started")
	}
	if !strings.Contains(err.Error(), "not allowed here") {
		t.Fatalf("refusal reason lost: %v", err)
	}
	if guard.finishedCode != nil {
		t.Fatal("a refused exec reported a finish")
	}
}

// The output cap bounds a runaway process. It applies to the SESSION, so a
// process cannot double its allowance by writing to both streams.
func TestOutputCapStopsARunawayProcess(t *testing.T) {
	guard := &stubGuard{maxOutput: 4096}
	m, c := newManager(t, guard)

	req := weavewire.ExecRequest{
		ExecID: "runaway",
		Argv:   []string{"sh", "-c", "while true; do echo AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA; done"},
	}
	if _, err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	exit := c.waitExit(t)
	if exit.Err == "" {
		t.Fatal("a capped exec finished without reporting why")
	}
	if !strings.Contains(exit.Err, "limit") {
		t.Fatalf("exit error does not explain the cap: %q", exit.Err)
	}
}

// A process that cannot start is an error on the START call, never an exit
// event: the host must be able to tell "did not run" from "ran and failed",
// because only the second has an exit code worth interpreting.
func TestFailureToStartIsNotAnExit(t *testing.T) {
	m, c := newManager(t, nil)
	_, err := m.Start(context.Background(), weavewire.ExecRequest{
		ExecID: "missing", Argv: []string{"definitely-not-a-real-binary-xyz"},
	})
	if err == nil {
		t.Fatal("starting a nonexistent program reported success")
	}
	select {
	case exit := <-c.done:
		t.Fatalf("a process that never started produced an exit event: %+v", exit)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDuplicateExecIDIsRefused(t *testing.T) {
	m, _ := newManager(t, nil)
	req := weavewire.ExecRequest{ExecID: "dup", Argv: []string{"sleep", "5"}}
	if _, err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	if _, err := m.Start(context.Background(), req); err == nil {
		t.Fatal("a duplicate exec id was accepted — two callers' streams would cross")
	}
}

// Stop must not leave processes running in a guest whose agent has gone.
func TestStopTerminatesRunningExecs(t *testing.T) {
	m, _ := newManager(t, nil)
	if _, err := m.Start(context.Background(), weavewire.ExecRequest{
		ExecID: "long", Argv: []string{"sleep", "120"},
	}); err != nil {
		t.Fatal(err)
	}

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not terminate a running exec")
	}
}

// A terminal exec must give the process a real controlling terminal: that is
// the whole difference from pipes, and a program that checks reports it.
func TestTTYExecGivesTheProcessATerminal(t *testing.T) {
	m, c := newManager(t, nil)
	if _, err := exec.LookPath("tty"); err != nil {
		t.Skip("no tty(1) here")
	}
	req := weavewire.ExecRequest{
		ExecID: "tty",
		Argv:   []string{"tty"},
		TTY:    true,
		Cols:   80,
		Rows:   24,
	}
	if _, err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	exit := c.waitExit(t)
	if exit.Code != 0 {
		t.Fatalf("tty(1) reported no terminal: %+v, out=%q", exit, c.out("tty"))
	}
	if !strings.Contains(c.out("tty"), "/dev/") {
		t.Fatalf("tty(1) did not name a terminal device: %q", c.out("tty"))
	}
}

type stubGuard struct {
	refuse       error
	maxOutput    int64
	finishedCode *int
}

func (g *stubGuard) Authorise(context.Context, string, weavewire.ExecRequest) (Limits, error) {
	return Limits{MaxOutputBytes: g.maxOutput}, g.refuse
}

func (g *stubGuard) Finished(
	_ context.Context,
	_ string,
	code int,
	_ error,
) {
	g.finishedCode = &code
}

func itoa(n int) string { return strconv.Itoa(n) }
