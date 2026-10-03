//go:build unix

package weaveexec

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

func TestUnixSignalNames(t *testing.T) {
	for name, want := range map[string]syscall.Signal{
		weavewire.SignalTerm: syscall.SIGTERM,
		"":                   syscall.SIGTERM,
		weavewire.SignalKill: syscall.SIGKILL,
		weavewire.SignalInt:  syscall.SIGINT,
	} {
		got, err := unixSignal(name)
		if err != nil || got != want {
			t.Errorf("unixSignal(%q) = %v, %v", name, got, err)
		}
	}
	if _, err := unixSignal("HUP"); err == nil {
		t.Error("an unmapped signal was accepted")
	}
}

// Dir and Env are applied, and Env replaces rather than extends.
func TestUnixStarterAppliesDirAndEnv(t *testing.T) {
	p, err := UnixStarter{}.Start(context.Background(), weavewire.ExecRequest{
		Argv: []string{"/bin/sh", "-c", "pwd; echo $ONLY"}, Dir: "/", Env: []string{"ONLY=this"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p.Stdout())
	if code, _, err := p.Wait(); code != 0 || err != nil {
		t.Fatalf("wait = %d, %v", code, err)
	}
	if got := strings.Fields(string(out)); len(got) != 2 || got[0] != "/" || got[1] != "this" {
		t.Fatalf("output = %q", out)
	}
	if err := p.Resize(80, 24); err == nil {
		t.Error("a pipe exec accepted a resize")
	}
	if p.Stdin() != nil {
		t.Error("an exec without stdin was given a pipe")
	}
	if err := p.Close(); err != nil {
		t.Error(err)
	}
}

func TestUnixStarterReportsATerminalThatCannotStart(t *testing.T) {
	if _, err := (UnixStarter{}).Start(context.Background(), weavewire.ExecRequest{
		Argv: []string{"definitely-not-a-real-binary-xyz"}, TTY: true,
	}); err == nil {
		t.Fatal("a missing program started under a terminal")
	}
}

// A process value whose command never started has no pid and cannot be
// signalled; it must say so rather than panic.
func TestUnstartedProcess(t *testing.T) {
	p := &unixProcess{cmd: exec.Command("true")}
	if p.PID() != 0 {
		t.Error("pid of an unstarted process")
	}
	if err := p.Signal(weavewire.SignalTerm); err == nil {
		t.Error("signalled an unstarted process")
	}
}

func TestUnixProcessRefusesAnUnmappedSignal(t *testing.T) {
	p, err := UnixStarter{}.Start(
		context.Background(),
		weavewire.ExecRequest{Argv: []string{"sleep", "5"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal("HUP"); err == nil {
		t.Error("an unmapped signal was delivered")
	}
	_ = p.Signal(weavewire.SignalKill)
	_, _, _ = p.Wait()
}

// A process that exits on its own with a code reports that code and no
// signal; one whose Wait fails for another reason reports the failure.
func TestUnixWaitOutcomes(t *testing.T) {
	p, err := UnixStarter{}.Start(
		context.Background(),
		weavewire.ExecRequest{Argv: []string{"sh", "-c", "exit 4"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if code, sig, err := p.Wait(); code != 4 || sig != "" || err != nil {
		t.Fatalf("wait = %d, %q, %v", code, sig, err)
	}
	// Waiting twice is the "for another reason" case os/exec reports.
	if code, _, err := p.Wait(); err == nil || code != -1 {
		t.Fatalf("second wait = %d, %v", code, err)
	}
}
