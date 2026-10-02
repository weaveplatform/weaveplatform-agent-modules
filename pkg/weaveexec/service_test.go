package weaveexec_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveexec"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavepolicy"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// These run on every OS: the starter is a scripted fake, so what is under test
// is the service — decoding, policy, audit, routing — not process creation.

type fakeProcess struct {
	out     io.Reader
	stdin   *closeBuffer
	exit    chan struct{}
	code    int
	waitErr error

	mu      sync.Mutex
	resized [][2]uint16
	signals []string
}

type closeBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
}

func (b *closeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *closeBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (p *fakeProcess) Stdout() io.Reader { return p.out }
func (p *fakeProcess) Stderr() io.Reader { return nil }
func (p *fakeProcess) Stdin() io.WriteCloser {
	if p.stdin == nil {
		return nil
	}
	return p.stdin
}
func (p *fakeProcess) PID() int { return 4242 }
func (p *fakeProcess) Resize(c, r uint16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resized = append(p.resized, [2]uint16{c, r})
	return nil
}

func (p *fakeProcess) Signal(name string) error {
	p.mu.Lock()
	p.signals = append(p.signals, name)
	p.mu.Unlock()
	select {
	case <-p.exit:
	default:
		close(p.exit)
	}
	return nil
}

func (p *fakeProcess) Wait() (int, string, error) {
	<-p.exit
	return p.code, "", p.waitErr
}
func (p *fakeProcess) Close() error { return nil }

// fakeStarter hands out a process that prints its argv and runs until
// signalled (or immediately exits when exitNow is set).
type fakeStarter struct {
	mu      sync.Mutex
	started []weavewire.ExecRequest
	procs   map[string]*fakeProcess
	exitNow bool
	err     error
}

func (s *fakeStarter) Start(
	_ context.Context,
	req weavewire.ExecRequest,
) (weaveexec.Process, error) {
	if s.err != nil {
		return nil, s.err
	}
	p := &fakeProcess{
		out:  strings.NewReader(strings.Join(req.Argv, " ")),
		exit: make(chan struct{}),
	}
	if req.Stdin {
		p.stdin = &closeBuffer{}
	}
	if s.exitNow {
		close(p.exit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, req)
	if s.procs == nil {
		s.procs = map[string]*fakeProcess{}
	}
	s.procs[req.ExecID] = p
	return p, nil
}

func (s *fakeStarter) proc(id string) *fakeProcess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[id]
}

func (s *fakeStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.started)
}

func withPolicy(doc string, err error) func(*weavemoduletest.Host) {
	return func(h *weavemoduletest.Host) { h.SetPolicy([]byte(doc), err) }
}

func waitExit(t *testing.T, h *weavemoduletest.Harness, id string) weavewire.ExecExit {
	t.Helper()
	var exit weavewire.ExecExit
	ok := h.T.WaitFor(10*time.Second, func(sent []modulesdk.Message) bool {
		for _, m := range sent {
			if m.Kind != weavewire.KindExecExit {
				continue
			}
			if json.Unmarshal(m.Data, &exit) == nil && exit.ExecID == id {
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("no exit for %s", id)
	}
	return exit
}

func audits(t *testing.T, h *weavemoduletest.Harness) []weavepolicy.ExecAudit {
	t.Helper()
	var out []weavepolicy.ExecAudit
	for _, p := range h.Host.Published() {
		if p.Topic != weavepolicy.AuditTopic {
			t.Fatalf("published on %q", p.Topic)
		}
		var a weavepolicy.ExecAudit
		if err := json.Unmarshal(p.Data, &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func TestServesExactlyTheExecContract(t *testing.T) {
	if err := weavemodule.CheckParity(weaveexec.NewService(&fakeStarter{})); err != nil {
		t.Fatal(err)
	}
}

func TestStartStreamsAndAuditsWithNoPolicy(t *testing.T) {
	starter := &fakeStarter{exitNow: true}
	h := weavemoduletest.Start(t, weaveexec.NewService(starter))

	var started weavewire.ExecStartResponse
	h.Decode(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "e1", Argv: []string{"echo", "hi"}},
		&started,
	)
	if started.ExecID != "e1" || started.PID != 4242 {
		t.Fatalf("start = %+v", started)
	}
	if exit := waitExit(t, h, "e1"); exit.Code != 0 || exit.Err != "" {
		t.Fatalf("exit = %+v", exit)
	}
	// Absent policy permits, but still leaves a record: one before the start,
	// one at the finish.
	got := audits(t, h)
	if len(got) != 2 || got[0].Refused != "" || got[0].Policy || got[1].ExitCode == nil {
		t.Fatalf("audits = %+v", got)
	}
}

func TestPolicyRefusalIsAuditedAndNothingStarts(t *testing.T) {
	starter := &fakeStarter{}
	h := weavemoduletest.Start(t, weaveexec.NewService(starter),
		withPolicy(`{"deny_argv0":["rm"]}`, nil))

	res := h.Call(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "no", Argv: []string{"rm", "-rf", "/"}},
	)
	if !strings.Contains(res.Err, "denied by policy") {
		t.Fatalf("err = %q", res.Err)
	}
	if starter.count() != 0 {
		t.Fatal("a refused exec was started")
	}
	got := audits(t, h)
	if len(got) != 1 || got[0].Refused == "" || !got[0].Policy {
		t.Fatalf("audits = %+v", got)
	}
}

// A policy that cannot be parsed refuses everything: falling back to allow-all
// would turn a typo into an open door.
func TestMalformedPolicyRefusesEverything(t *testing.T) {
	starter := &fakeStarter{}
	h := weavemoduletest.Start(t, weaveexec.NewService(starter), withPolicy(`{not json`, nil))
	if res := h.Call(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "x", Argv: []string{"true"}},
	); res.Err == "" {
		t.Fatal("a malformed policy permitted an exec")
	}
	if starter.count() != 0 {
		t.Fatal("started anyway")
	}
}

// No policy delivered yet is the disconnected-guest case: permit and audit.
func TestUndeliveredPolicyPermits(t *testing.T) {
	starter := &fakeStarter{exitNow: true}
	h := weavemoduletest.Start(t, weaveexec.NewService(starter),
		withPolicy("", errors.New("no policy yet")))
	if res := h.Call(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "ok", Argv: []string{"true"}},
	); res.Err != "" {
		t.Fatalf("err = %q", res.Err)
	}
	waitExit(t, h, "ok")
}

// The audit sink failing must not stop the exec.
func TestAuditFailureDoesNotBlockTheExec(t *testing.T) {
	starter := &fakeStarter{exitNow: true}
	h := weavemoduletest.Start(t, weaveexec.NewService(starter), func(host *weavemoduletest.Host) {
		host.FailPublish(func(string) error { return errors.New("bus down") })
	})
	if res := h.Call(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "a", Argv: []string{"true"}},
	); res.Err != "" {
		t.Fatalf("err = %q", res.Err)
	}
	waitExit(t, h, "a")
}

func TestStdinResizeAndSignalReachTheProcess(t *testing.T) {
	starter := &fakeStarter{}
	h := weavemoduletest.Start(t, weaveexec.NewService(starter))
	h.Decode(weavewire.KindExecStart, weavewire.ExecRequest{
		ExecID: "i", Argv: []string{"cat"}, Stdin: true, TTY: true,
	}, &weavewire.ExecStartResponse{})

	h.Notify(weavewire.KindExecStdin, weavewire.Chunk{StreamID: "i", Seq: 0, Data: []byte("hello")})
	h.Notify(weavewire.KindExecStdin, weavewire.Chunk{StreamID: "i", Seq: 1, EOF: true})
	if res := h.Call(
		weavewire.KindExecResize,
		weavewire.ExecResizeRequest{ExecID: "i", Cols: 100, Rows: 40},
	); res.Err != "" {
		t.Fatal(res.Err)
	}
	p := starter.proc("i")
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.stdin.mu.Lock()
		done := p.stdin.closed && p.stdin.buf.String() == "hello"
		p.stdin.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stdin never reached the process in order")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if res := h.Call(
		weavewire.KindExecSignal,
		weavewire.ExecSignalRequest{ExecID: "i", Signal: weavewire.SignalTerm},
	); res.Err != "" {
		t.Fatal(res.Err)
	}
	waitExit(t, h, "i")

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.resized) != 1 || p.resized[0] != [2]uint16{100, 40} {
		t.Fatalf("resizes = %v", p.resized)
	}
	if len(p.signals) == 0 || p.signals[0] != weavewire.SignalTerm {
		t.Fatalf("signals = %v", p.signals)
	}
}

func TestMalformedPayloadsAreRefused(t *testing.T) {
	h := weavemoduletest.Start(t, weaveexec.NewService(&fakeStarter{}))
	for _, kind := range []string{weavewire.KindExecStart, weavewire.KindExecResize, weavewire.KindExecSignal} {
		if res := h.Call(kind, "not an object"); res.Err == "" {
			t.Errorf("%s accepted a malformed payload", kind)
		}
	}
	// Stdin carries no id, so its failure is only logged; it must not crash
	// the dispatcher, which the next call proves.
	h.Notify(weavewire.KindExecStdin, "not a chunk")
	if res := h.Call(
		weavewire.KindExecResize,
		weavewire.ExecResizeRequest{ExecID: "none"},
	); res.Err == "" {
		t.Error("resizing an unknown exec succeeded")
	}
}

func TestStopKillsRunningExecs(t *testing.T) {
	starter := &fakeStarter{}
	svc := weaveexec.NewService(starter)
	// Stopping a service that was never registered must be safe.
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := weavemoduletest.Start(t, svc)
	h.Decode(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "long", Argv: []string{"sleep"}},
		&weavewire.ExecStartResponse{},
	)
	if err := h.Module.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := starter.proc("long")
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.signals) == 0 || p.signals[0] != weavewire.SignalKill {
		t.Fatalf("signals = %v", p.signals)
	}
}

func TestStartFailureIsAuditedAsFinished(t *testing.T) {
	starter := &fakeStarter{err: errors.New("no such file")}
	h := weavemoduletest.Start(t, weaveexec.NewService(starter))
	res := h.Call(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "m", Argv: []string{"missing"}},
	)
	if !strings.Contains(res.Err, "no such file") {
		t.Fatalf("err = %q", res.Err)
	}
	got := audits(t, h)
	if len(got) != 2 || got[1].ExitCode == nil || *got[1].ExitCode != -1 || got[1].Refused == "" {
		t.Fatalf("audits = %+v", got)
	}
}

func TestWaitErrorIsReportedOnTheExit(t *testing.T) {
	starter := &fakeStarter{}
	h := weavemoduletest.Start(t, weaveexec.NewService(starter))
	h.Decode(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "w", Argv: []string{"x"}},
		&weavewire.ExecStartResponse{},
	)
	p := starter.proc("w")
	p.waitErr = errors.New("wait failed")
	close(p.exit)
	if exit := waitExit(t, h, "w"); exit.Err != "wait failed" {
		t.Fatalf("exit = %+v", exit)
	}
}
