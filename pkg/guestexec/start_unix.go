//go:build unix

package guestexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// UnixStarter runs processes on macOS and Linux. The two are identical here —
// same os/exec, same pseudo-terminal API — so one implementation serves both
// modules rather than two that can drift.
type UnixStarter struct{}

// Start launches the process, with a pseudo-terminal when the host asked for
// one.
//
// The program is run directly, never through a shell: passing host-supplied
// argv to `sh -c` would make every argument containing a space or a semicolon
// an injection, and the host cannot escape correctly for three guest OSes.
//
// ctx is the exec's run context: the manager cancels it when the module stops,
// which kills the process with it.
func (UnixStarter) Start(ctx context.Context, req guestwire.ExecRequest) (Process, error) {
	//nolint:gosec // G204: running host-chosen argv is this capability's purpose; policy gates it.
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if len(req.Env) > 0 {
		cmd.Env = req.Env
	}

	if req.TTY {
		return startTTY(cmd, req)
	}
	return startPipes(cmd, req)
}

// startTTY runs the process under a pseudo-terminal, giving it a real
// controlling terminal — a prompt, line editing, job control. This is what
// `weave exec -it` and `weave ssh` need; without it most programs detect a pipe
// and switch to block buffering, so output arrives in lumps or not until exit.
func startTTY(cmd *exec.Cmd, req guestwire.ExecRequest) (Process, error) {
	size := &pty.Winsize{Cols: req.Cols, Rows: req.Rows}
	if size.Cols == 0 || size.Rows == 0 {
		// A zero-sized terminal makes full-screen programs misbehave in ways
		// that look like guestweave bugs. 80x24 is the conventional default.
		size.Cols, size.Rows = 80, 24
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("guestexec: opening a terminal: %w", err)
	}
	if err := pty.Setsize(ptmx, size); err != nil {
		closeAll(ptmx, tty)
		return nil, fmt.Errorf("guestexec: sizing the terminal: %w", err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		closeAll(ptmx, tty)
		return nil, fmt.Errorf("guestexec: starting under a terminal: %w", err)
	}
	// One stream: a terminal merges stdout and stderr, as a terminal does.
	p := &unixProcess{cmd: cmd, ptmx: ptmx, in: ptmx, waited: make(chan struct{})}
	p.out = stampedReader{r: ptmx, last: &p.lastRead}
	p.lastRead.Store(time.Now().UnixNano())
	go p.reapTTY(tty)
	return p, nil
}

// reapTTY waits for the child, then releases our copy of the terminal's child
// side once its output has drained.
//
// Our copy is held open on purpose. On macOS, the last close of a terminal's
// child side throws away output nobody has read yet, so a command that prints
// and exits at once — tty(1), echo — would otherwise lose everything it wrote.
// Holding it keeps the output; closing it after the drain is what ends the
// stream, as the child's exit alone would have.
func (p *unixProcess) reapTTY(tty *os.File) {
	p.waitErr = p.cmd.Wait()
	close(p.waited)
	settle(&p.lastRead)
	tty.Close() //nolint:errcheck,gosec // nothing useful to do about a failed close of our copy
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		f.Close() //nolint:errcheck,gosec // cleanup after a failure already being reported
	}
}

// startPipes runs the process with plain pipes and separate stdout/stderr.
func startPipes(cmd *exec.Cmd, req guestwire.ExecRequest) (Process, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("guestexec: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("guestexec: stderr pipe: %w", err)
	}

	var stdin io.WriteCloser
	if req.Stdin {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return nil, fmt.Errorf("guestexec: stdin pipe: %w", err)
		}
	} else {
		// Not merely leaving Stdin nil: a nil Stdin gives the child /dev/null,
		// which is what we want — a command that reads input should see EOF and
		// exit rather than block forever on a stream the host will never write.
		cmd.Stdin = nil
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("guestexec: starting: %w", err)
	}
	return &unixProcess{cmd: cmd, out: stdout, errOut: stderr, in: stdin}, nil
}

type unixProcess struct {
	cmd *exec.Cmd
	// ptmx is the terminal master when running under a pseudo-terminal, and
	// nil otherwise. It is both the output and the input in that mode.
	ptmx   *os.File
	out    io.Reader
	errOut io.Reader
	in     io.WriteCloser

	// mu guards use of ptmx against the close that ends the session.
	//
	// Resize and Close arrive on different goroutines — a terminal resize is
	// driven by the operator's SIGWINCH, the close by the process exiting — so
	// they collide whenever a window is resized as a command finishes. Without
	// this they race on the file's own state, and worse: once the descriptor is
	// closed the kernel may hand the same number to something else, and the
	// resize would then be applied to an unrelated file. The race detector found
	// it on its first run against this package.
	mu     sync.Mutex
	closed bool

	// Terminal mode only: the child is reaped by reapTTY, which must call
	// cmd.Wait itself so it can release the terminal on exit; Wait reports
	// what it recorded. waited is nil in pipe mode.
	waited   chan struct{}
	waitErr  error
	lastRead atomic.Int64
}

func (p *unixProcess) Stdout() io.Reader     { return p.out }
func (p *unixProcess) Stderr() io.Reader     { return p.errOut }
func (p *unixProcess) Stdin() io.WriteCloser { return p.in }
func (p *unixProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *unixProcess) Resize(cols, rows uint16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ptmx == nil {
		return errNoTerminal
	}
	// A resize arriving after the session ended is late, not wrong: the operator
	// dragged a window while the command was exiting. Say so and do nothing,
	// rather than touching a closed descriptor.
	if p.closed {
		return errEnded
	}
	if err := pty.Setsize(p.ptmx, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		return fmt.Errorf("guestexec: resizing: %w", err)
	}
	return nil
}

func (p *unixProcess) Signal(name string) error {
	if p.cmd.Process == nil {
		return errNotRunning
	}
	sig, err := unixSignal(name)
	if err != nil {
		return err
	}
	if err := p.cmd.Process.Signal(sig); err != nil {
		return fmt.Errorf("guestexec: signalling: %w", err)
	}
	return nil
}

func (p *unixProcess) Wait() (int, string, error) {
	var err error
	if p.waited != nil {
		<-p.waited
		err = p.waitErr
	} else {
		err = p.cmd.Wait()
	}
	if err == nil {
		return 0, "", nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1, "", fmt.Errorf("guestexec: waiting: %w", err)
	}
	// A signalled process gets the conventional 128+n code as well as the
	// signal name, so a caller that only inspects the code still sees failure.
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		sig := status.Signal()
		return 128 + int(sig), sig.String(), nil
	}
	return exitErr.ExitCode(), "", nil
}

func (p *unixProcess) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.ptmx == nil {
		return nil
	}
	p.closed = true
	if err := p.ptmx.Close(); err != nil {
		return fmt.Errorf("guestexec: closing the terminal: %w", err)
	}
	return nil
}

// unixSignal maps a portable signal name onto this OS's signal. Names rather
// than numbers cross the wire because the numbers differ between guest OSes.
func unixSignal(name string) (os.Signal, error) {
	switch name {
	case guestwire.SignalTerm, "":
		return syscall.SIGTERM, nil
	case guestwire.SignalKill:
		return syscall.SIGKILL, nil
	case guestwire.SignalInt:
		return syscall.SIGINT, nil
	default:
		return nil, fmt.Errorf("%w: %q", errUnsupportedSignal, name)
	}
}
