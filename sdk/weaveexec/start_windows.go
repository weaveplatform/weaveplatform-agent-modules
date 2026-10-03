//go:build windows

package weaveexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// WindowsStarter runs processes in a Windows guest.
//
// Interactive execs go through a pseudo-console (see conpty_windows.go);
// everything else uses plain pipes. The pseudo-console path needs
// CreatePseudoConsole/ResizePseudoConsole, which go-bindings-win32 gained in
// v0.3.0 — before that a TTY request was refused rather than silently
// downgraded to pipes.
type WindowsStarter struct{}

// Start launches the process. As on Unix, argv is run directly and never
// through cmd.exe: quoting host-supplied arguments for a shell is exactly the
// injection surface worth not having.
//
// ctx is the exec's run context: the manager cancels it when the module stops,
// which kills a pipe-attached process with it. A pseudo-console process is
// created outside os/exec and is killed by the manager's Stop instead.
func (WindowsStarter) Start(ctx context.Context, req weavewire.ExecRequest) (Process, error) {
	if req.TTY {
		return startConPTY(req)
	}

	//nolint:gosec // G204: running host-chosen argv is this capability's purpose; policy gates it.
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if len(req.Env) > 0 {
		cmd.Env = req.Env
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("weaveexec: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("weaveexec: stderr pipe: %w", err)
	}
	var stdin io.WriteCloser
	if req.Stdin {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return nil, fmt.Errorf("weaveexec: stdin pipe: %w", err)
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("weaveexec: starting: %w", err)
	}
	return &windowsProcess{cmd: cmd, out: stdout, errOut: stderr, in: stdin}, nil
}

type windowsProcess struct {
	cmd    *exec.Cmd
	out    io.Reader
	errOut io.Reader
	in     io.WriteCloser
}

func (p *windowsProcess) Stdout() io.Reader     { return p.out }
func (p *windowsProcess) Stderr() io.Reader     { return p.errOut }
func (p *windowsProcess) Stdin() io.WriteCloser { return p.in }
func (p *windowsProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Resize is meaningless without a terminal: this process has pipes, and a
// caller that resizes one is confused about what it started.
func (p *windowsProcess) Resize(uint16, uint16) error {
	return errNoTerminal
}

// Signal terminates the process.
//
// Windows has no signals. TERM and KILL both become a hard termination, which
// is honest — there is no graceful variant to offer — and INT is refused rather
// than pretended, because a caller sending INT expects the process to get a
// chance to clean up and it would not.
func (p *windowsProcess) Signal(name string) error {
	if p.cmd.Process == nil {
		return errNotRunning
	}
	switch name {
	case weavewire.SignalTerm, weavewire.SignalKill, "":
		if err := p.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("weaveexec: terminating: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("%w: %q has no Windows equivalent", errUnsupportedSignal, name)
	}
}

func (p *windowsProcess) Wait() (int, string, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, "", nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1, "", fmt.Errorf("weaveexec: waiting: %w", err)
	}
	return exitErr.ExitCode(), "", nil
}

func (p *windowsProcess) Close() error { return nil }
