//go:build windows

package guestexec

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// These run real processes on windows-latest. Every blocking step is bounded,
// so a regression shows as a failure naming the step, not a hung job.

const step = 30 * time.Second

func bounded[T any](t *testing.T, what string, fn func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v
	case <-time.After(step):
		t.Fatalf("%s did not finish within %s", what, step)
		var zero T
		return zero
	}
}

type waited struct {
	code int
	sig  string
	err  error
}

func waitProc(t *testing.T, p Process) waited {
	t.Helper()
	return bounded(t, "Wait", func() waited {
		code, sig, err := p.Wait()
		return waited{code, sig, err}
	})
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	return bounded(t, "reading output", func() string {
		b, _ := io.ReadAll(r)
		return string(b)
	})
}

func cmdExe() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return root + `\System32\cmd.exe`
	}
	return "cmd.exe"
}

func TestWindowsPipesStdoutStderrAndExitCode(t *testing.T) {
	p, err := WindowsStarter{}.Start(context.Background(), guestwire.ExecRequest{
		Argv: []string{cmdExe(), "/c", "echo out& echo err 1>&2& exit 3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.PID() == 0 {
		t.Error("no pid")
	}
	errOut := make(chan string, 1)
	go func() { b, _ := io.ReadAll(p.Stderr()); errOut <- string(b) }()
	if out := readAll(t, p.Stdout()); strings.TrimSpace(out) != "out" {
		t.Errorf("stdout = %q", out)
	}
	if got := strings.TrimSpace(<-errOut); got != "err" {
		t.Errorf("stderr = %q", got)
	}
	if w := waitProc(t, p); w.code != 3 || w.err != nil || w.sig != "" {
		t.Fatalf("wait = %+v", w)
	}
	// A second Wait is os/exec's "already waited" error, not an exit code.
	if code, _, err := p.Wait(); err == nil || code != -1 {
		t.Fatalf("second wait = %d, %v", code, err)
	}
}

func TestWindowsPipesStdinDirAndEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+`\marker.txt`, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := WindowsStarter{}.Start(context.Background(), guestwire.ExecRequest{
		Argv: []string{cmdExe(), "/c", "dir /b& echo %ONLY%& findstr x"},
		Dir:  dir,
		// Env replaces the environment, so it carries what cmd itself needs.
		Env: []string{
			"ONLY=this",
			"SystemRoot=" + os.Getenv("SystemRoot"),
			"PATH=" + os.Getenv("PATH"),
		},
		Stdin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := io.WriteString(p.Stdin(), "has x\r\nnope\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := p.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	out := readAll(t, p.Stdout())
	waitProc(t, p)
	for _, want := range []string{"marker.txt", "this", "has x"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "nope") {
		t.Errorf("stdin filtering failed: %q", out)
	}
}

func TestWindowsPipesSignals(t *testing.T) {
	p, err := WindowsStarter{}.Start(context.Background(), guestwire.ExecRequest{
		Argv: []string{cmdExe(), "/c", "ping -n 60 127.0.0.1 >NUL 2>NUL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Resize(80, 24); err == nil {
		t.Error("a pipe exec accepted a resize")
	}
	if err := p.Signal(guestwire.SignalInt); err == nil {
		t.Error("INT was pretended rather than refused")
	}
	if err := p.Signal(guestwire.SignalKill); err != nil {
		t.Fatal(err)
	}
	if w := waitProc(t, p); w.code == 0 {
		t.Fatalf("a killed process reported success: %+v", w)
	}
}

func TestWindowsStartFailures(t *testing.T) {
	if _, err := (WindowsStarter{}).Start(context.Background(), guestwire.ExecRequest{
		Argv: []string{"definitely-not-a-real-binary-xyz"},
	}); err == nil {
		t.Fatal("a missing program started")
	}
	p := &windowsProcess{cmd: exec.Command("cmd.exe")}
	if p.PID() != 0 {
		t.Error("pid of an unstarted process")
	}
	if err := p.Signal(guestwire.SignalTerm); err == nil {
		t.Error("signalled an unstarted process")
	}
	if p.Close() != nil {
		t.Error("close")
	}
}

// The whole pipeline — manager, relay, chunked events — over the Windows
// starter, as the exec module runs it.
func TestWindowsManagerRelaysAnExec(t *testing.T) {
	rec := recordingEmitter{exits: make(chan guestwire.ExecExit, 1)}
	m := New(WindowsStarter{}, rec, nil, nil)
	if _, err := m.Start(context.Background(), guestwire.ExecRequest{
		ExecID: "w1", Argv: []string{cmdExe(), "/c", "exit 5"},
	}); err != nil {
		t.Fatal(err)
	}
	exit := bounded(t, "the exit event", func() guestwire.ExecExit { return <-rec.exits })
	if exit.Code != 5 || exit.Err != "" {
		t.Fatalf("exit = %+v", exit)
	}
}
