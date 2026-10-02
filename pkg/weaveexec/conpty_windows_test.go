//go:build windows

package weaveexec

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// startConsole starts req under a pseudo-console. A runner image without ConPTY
// support (it needs Windows 10 1809 or later) skips rather than fails: that
// is the platform's absence, not this code's bug. Any later failure is real.
func startConsole(t *testing.T, req weavewire.ExecRequest) Process {
	t.Helper()
	req.TTY = true
	p, err := WindowsStarter{}.Start(context.Background(), req)
	if err != nil && strings.Contains(err.Error(), "CreatePseudoConsole") {
		t.Skipf("no pseudo-console on this runner: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// The process's exit must end its output. A pseudo-console outlives its
// client, so without closing it on exit the relay — which drains output before
// reporting the exit — would wait forever.
func TestConPTYOutputEndsWhenTheProcessExits(t *testing.T) {
	p := startConsole(t, weavewire.ExecRequest{
		Argv: []string{cmdExe(), "/c", "echo", "MARKER", `a b "c"`},
		Env:  []string{"SystemRoot=" + systemRoot(), "MARK=1"},
		Dir:  systemRoot(),
	})
	if p.Stderr() != nil {
		t.Error("a terminal has one merged stream")
	}
	if p.PID() == 0 {
		t.Error("no pid")
	}
	out := readAll(t, p.Stdout())
	if !strings.Contains(out, "MARKER") {
		t.Errorf("output %q lacks the echoed arguments", out)
	}
	if w := waitProc(t, p); w.code != 0 || w.err != nil {
		t.Fatalf("wait = %+v", w)
	}
	if err := p.Resize(100, 30); err == nil {
		t.Error("resized a console whose process had exited")
	}
	if err := p.Signal(weavewire.SignalTerm); err != nil {
		t.Errorf("signalling an exited process: %v", err)
	}
}

func TestConPTYResizeInputAndTermination(t *testing.T) {
	p := startConsole(
		t,
		weavewire.ExecRequest{Argv: []string{cmdExe()}, Cols: 0, Rows: 0, Stdin: true},
	)
	go func() { _, _ = io.Copy(io.Discard, p.Stdout()) }()

	if err := p.Resize(0, 10); err == nil {
		t.Error("a zero-width console was accepted")
	}
	if err := p.Resize(120, 40); err != nil {
		t.Errorf("resize: %v", err)
	}
	if _, err := io.WriteString(p.Stdin(), "echo hello\r\n"); err != nil {
		t.Errorf("writing input: %v", err)
	}
	if err := p.Signal(weavewire.SignalInt); err == nil {
		t.Error("INT was pretended rather than refused")
	}
	if err := p.Signal(weavewire.SignalKill); err != nil {
		t.Fatal(err)
	}
	if w := waitProc(t, p); w.code != 1 {
		t.Fatalf("a terminated console process exited %+v", w)
	}
}

// Close on a running process must not hang: it terminates the child and
// releases the console.
func TestConPTYCloseWhileRunning(t *testing.T) {
	p := startConsole(t, weavewire.ExecRequest{Argv: []string{cmdExe()}, Stdin: true})
	bounded(t, "Close", func() error { return p.Close() })
	bounded(t, "a second Close", func() error { return p.Close() })
}

// End to end through the manager, as `weave exec -t` runs it.
func TestConPTYThroughTheManager(t *testing.T) {
	rec := recordingEmitter{exits: make(chan weavewire.ExecExit, 1)}
	m := New(WindowsStarter{}, rec, nil, nil)
	_, err := m.Start(context.Background(), weavewire.ExecRequest{
		ExecID: "tty", Argv: []string{cmdExe(), "/c", "exit 7"}, TTY: true, Cols: 80, Rows: 24,
	})
	if err != nil && strings.Contains(err.Error(), "CreatePseudoConsole") {
		t.Skipf("no pseudo-console on this runner: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	exit := bounded(t, "the exit event", func() weavewire.ExecExit { return <-rec.exits })
	if exit.Code != 7 {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestConPTYStartFailure(t *testing.T) {
	_, err := WindowsStarter{}.Start(context.Background(), weavewire.ExecRequest{
		Argv: []string{"definitely-not-a-real-binary-xyz"}, TTY: true,
	})
	if err == nil {
		t.Fatal("a missing program started under a pseudo-console")
	}
}

func TestCommandLineAndEnvironmentEncoding(t *testing.T) {
	if got := commandLineOf(
		[]string{`C:\Program Files\x.exe`, `a "b"`, "c"},
	); got != `"C:\Program Files\x.exe" "a \"b\"" c` {
		t.Errorf("command line = %s", got)
	}
	block, err := environmentBlock([]string{"A=1", "B=2"})
	if err != nil {
		t.Fatal(err)
	}
	// "A=1\0B=2\0\0": each entry NUL-terminated, then one more NUL.
	if len(block) != 9 || block[3] != 0 || block[7] != 0 || block[8] != 0 {
		t.Errorf("environment block = %v", block)
	}
	if _, err := environmentBlock([]string{"BAD=\x00"}); err == nil {
		t.Error("an entry with an embedded NUL was encoded")
	}
}

func systemRoot() string {
	if root := strings.TrimSuffix(cmdExe(), `\System32\cmd.exe`); root != cmdExe() {
		return root
	}
	return `C:\Windows`
}
