//go:build windows

package guestexec

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/console"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/pipes"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/threading"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// Interactive exec on Windows, via a pseudo-console.
//
// This is the Windows half of `weave exec -it` and `weave ssh`. Without a
// terminal a program detects a pipe and switches to block buffering, so output
// arrives in lumps or not until exit, and there is no line editing, no job
// control and no way to resize.
//
// It cannot go through os/exec. Attaching a pseudo-console means passing
// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE in an extended STARTUPINFOEX, and Go's
// syscall.SysProcAttr exposes no attribute list — so the process is created
// directly through the house bindings and its lifetime managed here.

// startConPTY runs a process attached to a new pseudo-console.
func startConPTY(req guestwire.ExecRequest) (Process, error) {
	size := consoleSize(req.Cols, req.Rows)
	if size.X == 0 || size.Y == 0 {
		// A zero-sized console makes full-screen programs misbehave in ways
		// that look like guestweave bugs. 80x24 is the conventional default.
		size = console.COORD{X: 80, Y: 24}
	}

	// Two pipes: what the host types goes in one end, what the console
	// produces comes out the other. The pseudo-console takes the far ends.
	var inputRead, inputWrite, outputRead, outputWrite foundation.HANDLE
	if err := pipes.CreatePipe(&inputRead, &inputWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("creating the console input pipe: %w", err)
	}
	if err := pipes.CreatePipe(&outputRead, &outputWrite, nil, 0); err != nil {
		closeHandles(inputRead, inputWrite)
		return nil, fmt.Errorf("creating the console output pipe: %w", err)
	}

	var pc console.HPCON
	if err := console.CreatePseudoConsole(size, inputRead, outputWrite, 0, &pc); err != nil {
		closeHandles(inputRead, inputWrite, outputRead, outputWrite)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	// The pseudo-console has duplicated the ends it was given; holding our
	// copies open would keep the console alive after the child exits and the
	// output read would never see EOF.
	closeHandles(inputRead, outputWrite)

	proc, err := spawnAttached(req, pc)
	if err != nil {
		console.ClosePseudoConsole(pc)
		closeHandles(inputWrite, outputRead)
		return nil, err
	}

	p := &conptyProcess{
		pc:      pc,
		process: proc.HProcess,
		thread:  proc.HThread,
		pid:     int(proc.DwProcessId),
		out:     os.NewFile(uintptr(outputRead), "conpty-out"),
		in:      os.NewFile(uintptr(inputWrite), "conpty-in"),
		exited:  make(chan struct{}),
	}
	p.lastRead.Store(time.Now().UnixNano())
	go p.watch()
	return p, nil
}

// spawnAttached creates the process with the pseudo-console attached.
func spawnAttached(
	req guestwire.ExecRequest,
	pc console.HPCON,
) (threading.PROCESS_INFORMATION, error) {
	var info threading.PROCESS_INFORMATION

	attributes, free, err := pseudoConsoleAttributes(pc)
	if err != nil {
		return info, err
	}
	defer free()

	startup := threading.STARTUPINFOEXW{LpAttributeList: attributes}
	startup.StartupInfo.Cb = uint32(unsafe.Sizeof(startup))

	commandLine, err := syscall.UTF16PtrFromString(commandLineOf(req.Argv))
	if err != nil {
		return info, fmt.Errorf("building the command line: %w", err)
	}

	var environment unsafe.Pointer
	if len(req.Env) > 0 {
		block, err := environmentBlock(req.Env)
		if err != nil {
			return info, err
		}
		environment = unsafe.Pointer(&block[0])
	}

	// A nil directory inherits ours; an empty string would be passed through
	// as "" and refused.
	var dir *string
	if req.Dir != "" {
		dir = &req.Dir
	}

	// bInheritHandles is false on purpose: the child reaches its console
	// through the attribute list, not through inherited handles, so nothing
	// else of ours leaks into it. The application name is nil so the first
	// token of the command line is resolved against PATH, as os/exec does.
	if err := threading.CreateProcess(
		nil, foundation.PWSTR(commandLine), nil, nil, false,
		threading.EXTENDED_STARTUPINFO_PRESENT|threading.CREATE_UNICODE_ENVIRONMENT,
		environment, dir,
		&startup.StartupInfo, &info,
	); err != nil {
		return info, fmt.Errorf("starting %q under a pseudo-console: %w", req.Argv[0], err)
	}
	return info, nil
}

// pseudoConsoleAttributes builds a one-entry attribute list carrying the
// pseudo-console, and a function that frees it.
func pseudoConsoleAttributes(
	pc console.HPCON,
) (threading.LPPROC_THREAD_ATTRIBUTE_LIST, func(), error) {
	// Sized by asking: the first call is EXPECTED to fail with
	// ERROR_INSUFFICIENT_BUFFER, which is how it reports the size it needs.
	var size uintptr
	err := threading.InitializeProcThreadAttributeList(0, 1, &size)
	if size == 0 {
		return 0, nil, fmt.Errorf("sizing the process attribute list: %w", err)
	}

	buffer := make([]byte, size)
	list := threading.LPPROC_THREAD_ATTRIBUTE_LIST(unsafe.Pointer(&buffer[0]))
	if err := threading.InitializeProcThreadAttributeList(list, 1, &size); err != nil {
		return 0, nil, fmt.Errorf("initialising the process attribute list: %w", err)
	}

	free := func() {
		threading.DeleteProcThreadAttributeList(list)
		// Keep the backing array alive until the list is deleted: the OS holds
		// a pointer into it for the lifetime of the list, and Go is otherwise
		// free to collect a slice nothing references.
		runtime.KeepAlive(buffer)
	}

	// The attribute's value is the HPCON itself, not a pointer to it — the
	// documented calling convention. Reinterpreting through its address
	// rather than converting the integer keeps vet's unsafe.Pointer check
	// meaningful everywhere else.
	value := *(*unsafe.Pointer)(unsafe.Pointer(&pc))
	if err := threading.UpdateProcThreadAttribute(list, 0,
		uintptr(threading.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE),
		value, unsafe.Sizeof(pc), nil, nil); err != nil {
		free()
		return 0, nil, fmt.Errorf("attaching the pseudo-console: %w", err)
	}
	return list, free, nil
}

// conptyProcess is a process running under a pseudo-console.
type conptyProcess struct {
	pc      console.HPCON
	process foundation.HANDLE
	thread  foundation.HANDLE
	pid     int
	out     *os.File
	in      *os.File

	// lastRead is when output was last read (unix nanos), so the console is
	// closed only once the final frame has drained.
	lastRead atomic.Int64

	// exited closes once the child has gone and code/waitErr are final.
	exited  chan struct{}
	code    int
	waitErr error

	consoleOnce sync.Once
	closeOnce   sync.Once
}

// watch waits for the child, records how it ended, then closes the console.
//
// The console outlives its last client: until it is closed, the output pipe
// never reports end of stream, so a reader draining it — the exec relay, which
// waits for output to drain before it reports the exit — would wait forever on
// a process that has already gone. Closing it here, once the child exits, is
// what turns the child's exit into the end of its output.
func (p *conptyProcess) watch() {
	if _, err := threading.WaitForSingleObject(p.process, threading.INFINITE); err != nil {
		p.code, p.waitErr = -1, fmt.Errorf("waiting for the process: %w", err)
	} else {
		var code uint32
		if err := threading.GetExitCodeProcess(p.process, &code); err != nil {
			p.code, p.waitErr = -1, fmt.Errorf("reading the exit code: %w", err)
		} else {
			p.code = int(int32(code))
		}
	}
	close(p.exited)
	p.settle()
	p.closeConsole()
}

// Settling bounds, vars so tests can shorten them.
var (
	settleQuiet = 200 * time.Millisecond
	settleMax   = 2 * time.Second
)

// settle waits for the output to go quiet before the console is closed. The
// console renders asynchronously: a child that prints and exits at once has
// its text still inside conhost when it exits, and closing the console then
// discards it. A short quiet period after the exit lets the final frame reach
// the pipe; the cap keeps a reader that has stopped reading from stalling it.
func (p *conptyProcess) settle() {
	deadline := time.Now().Add(settleMax)
	for time.Now().Before(deadline) {
		if time.Since(time.Unix(0, p.lastRead.Load())) >= settleQuiet {
			return
		}
		time.Sleep(settleQuiet / 8)
	}
}

// closeConsole releases the pseudo-console once. Closing it can wait for its
// final output to be read; the relay is reading, and Close breaks the pipe
// first, so neither caller waits on nobody.
func (p *conptyProcess) closeConsole() {
	p.consoleOnce.Do(func() { console.ClosePseudoConsole(p.pc) })
}

// Stdout carries everything: a terminal merges the two streams, as a terminal
// does, so there is no separate stderr to report.
func (p *conptyProcess) Stdout() io.Reader     { return conptyReader{p} }
func (p *conptyProcess) Stderr() io.Reader     { return nil }
func (p *conptyProcess) Stdin() io.WriteCloser { return p.in }
func (p *conptyProcess) PID() int              { return p.pid }

// Resize changes the console size, for the host's SIGWINCH.
func (p *conptyProcess) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return fmt.Errorf("%w: %dx%d", errZeroConsole, cols, rows)
	}
	select {
	case <-p.exited:
		// The console may already be released; resizing it would touch a
		// handle that is no longer ours.
		return errEnded
	default:
	}
	if err := console.ResizePseudoConsole(p.pc, consoleSize(cols, rows)); err != nil {
		return fmt.Errorf("guestexec: resizing the console: %w", err)
	}
	return nil
}

// Signal terminates the process. Windows has no signals: TERM and KILL both
// become a hard termination, which is honest — there is no graceful variant to
// offer — and INT is refused rather than pretended, because a caller sending it
// expects the process to get a chance to clean up and it would not.
func (p *conptyProcess) Signal(name string) error {
	switch name {
	case guestwire.SignalTerm, guestwire.SignalKill, "":
		select {
		case <-p.exited:
			return nil // already gone; the exit is on its way
		default:
		}
		if err := threading.TerminateProcess(p.process, 1); err != nil {
			return fmt.Errorf("guestexec: terminating: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("%w: %q has no Windows equivalent", errUnsupportedSignal, name)
	}
}

// conptyReader stamps each read so settle can tell when output has drained.
type conptyReader struct{ p *conptyProcess }

func (r conptyReader) Read(b []byte) (int, error) {
	n, err := r.p.out.Read(b)
	if n > 0 {
		r.p.lastRead.Store(time.Now().UnixNano())
	}
	return n, err //nolint:wrapcheck // io.Reader contract: io.EOF must pass through unwrapped
}

// Wait blocks until the process exits and reports its code.
func (p *conptyProcess) Wait() (int, string, error) {
	<-p.exited
	return p.code, "", p.waitErr
}

// Close releases the console and the handles, terminating the child if it is
// still running: a console with no owner would otherwise keep it alive.
//
// The output pipe is closed before the console, so a ClosePseudoConsole still
// waiting to deliver a final frame has a broken pipe to give up on rather than
// a reader that will never come.
func (p *conptyProcess) Close() error {
	p.closeOnce.Do(func() {
		_ = p.in.Close()
		select {
		case <-p.exited:
		default:
			_ = threading.TerminateProcess(p.process, 1)
			<-p.exited
		}
		_ = p.out.Close()
		p.closeConsole()
		closeHandles(p.thread, p.process)
	})
	return nil
}

var errZeroConsole = errors.New("guestexec: refusing a zero-sized console")

// consoleSize converts a terminal size to a console COORD, clamping what a
// COORD cannot hold rather than letting it wrap negative.
func consoleSize(cols, rows uint16) console.COORD {
	clamp := func(v uint16) int16 { return int16(min(v, math.MaxInt16)) } //nolint:gosec // G115: clamped on this line
	return console.COORD{X: clamp(cols), Y: clamp(rows)}
}

func closeHandles(handles ...foundation.HANDLE) {
	for _, h := range handles {
		if h != 0 {
			_ = foundation.CloseHandle(h)
		}
	}
}

// commandLineOf joins argv into a Windows command line.
//
// CreateProcess takes one string and the callee re-splits it, so each argument
// is escaped to survive that round trip — the same rule os/exec applies, which
// is why syscall.EscapeArg is used rather than a hand-rolled quoting pass.
func commandLineOf(argv []string) string {
	escaped := make([]string, len(argv))
	for i, arg := range argv {
		escaped[i] = syscall.EscapeArg(arg)
	}
	return strings.Join(escaped, " ")
}

// environmentBlock builds the doubly-NUL-terminated UTF-16 block CreateProcess
// expects from "KEY=VALUE" strings.
func environmentBlock(env []string) ([]uint16, error) {
	var block []uint16
	for _, entry := range env {
		encoded, err := syscall.UTF16FromString(entry)
		if err != nil {
			return nil, fmt.Errorf("encoding environment entry: %w", err)
		}
		block = append(block, encoded...) // each already NUL-terminated
	}
	return append(block, 0), nil // the extra NUL ends the block
}
