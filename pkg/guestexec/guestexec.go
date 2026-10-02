// Package guestexec runs processes inside a guest and streams their stdio back
// over the hypervisor channel.
//
// The OS-agnostic parts — the session registry, the chunking, the exit
// accounting, the output cap — live here so all three guest OSes behave
// identically. What differs per OS is only how a process is started with a
// terminal attached, which is the Starter interface below.
//
// # Why stdio is chunked rather than streamed
//
// There is one channel per guest and core owns it (the frame protocol has no
// resynchronisation, so a second wire is not an option). Every running exec
// therefore shares it, multiplexed by exec id. Output is emitted as
// guestwire.Chunk events as it is produced, so a long-running command reports
// progress instead of arriving all at once when it finishes.
package guestexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestagent"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// Failures a caller may branch on.
var (
	// ErrBadRequest: the exec request is missing something it must carry.
	ErrBadRequest = errors.New("guestexec: bad request")
	// ErrNoSuchExec: the exec id names no running process.
	ErrNoSuchExec = errors.New("guestexec: no such exec")
	// ErrOutputLimit: the exec streamed more than its policy allows.
	ErrOutputLimit = errors.New("guestexec: output limit exceeded")
	// ErrNoStdin: input was sent to an exec started without stdin.
	ErrNoStdin = errors.New("guestexec: exec was not started with stdin")
	// ErrExecFailed: the process could not be run to completion.
	ErrExecFailed = errors.New("guestexec: exec failed")
)

// OS-level refusals, shared by the starters.
var (
	errNoTerminal        = errors.New("guestexec: exec has no terminal to resize")
	errEnded             = errors.New("guestexec: exec has ended")
	errNotRunning        = errors.New("guestexec: process is not running")
	errUnsupportedSignal = errors.New("guestexec: signal not supported on this OS")
)

// Process is a started process, however the OS started it.
type Process interface {
	// Stdout and Stderr are the streams to relay. Under a pseudo-terminal
	// there is only one — the terminal merges them, as a terminal does — and
	// Stderr is nil.
	Stdout() io.Reader
	Stderr() io.Reader
	// Stdin accepts the host's input, or is nil when none was requested.
	Stdin() io.WriteCloser
	// PID identifies the process to an operator inside the guest.
	PID() int
	// Resize changes the terminal size. It is a no-op without a terminal.
	Resize(cols, rows uint16) error
	// Signal delivers a portable signal name (guestwire.SignalTerm etc.).
	Signal(name string) error
	// Wait blocks until the process exits, returning its exit code and the
	// signal that killed it, if any.
	Wait() (code int, signal string, err error)
	// Close releases the OS resources — the terminal master in particular,
	// which otherwise leaks a file descriptor per exec.
	Close() error
}

// Starter launches a process. Each guest OS provides one.
type Starter interface {
	Start(ctx context.Context, req guestwire.ExecRequest) (Process, error)
}

// Limits are the constraints an authorisation places on one exec.
type Limits struct {
	// MaxOutputBytes caps the combined stdout+stderr this exec may stream.
	// Zero means no cap. The cap is per SESSION, so a process cannot double
	// its allowance by splitting output across the two streams.
	MaxOutputBytes int64
}

// Guard authorises an exec and records it. guestpolicy provides the
// implementation; it is an interface here so this package does not have to know
// where policy comes from.
//
// Limits are returned per call rather than read back from the Guard later:
// several execs are authorised concurrently, so a limit stashed on the Guard
// between Authorise and use would be whichever exec asked last.
type Guard interface {
	// Authorise reports why an exec may not run, or nil to permit it along
	// with the limits that apply to it. Called before anything is started.
	Authorise(ctx context.Context, execID string, req guestwire.ExecRequest) (Limits, error)
	// Finished records the outcome. Best-effort; never blocks the exit path.
	Finished(ctx context.Context, execID string, code int, cause error)
}

// Manager owns every running exec for one module.
type Manager struct {
	start Starter
	guard Guard
	emit  guestagent.Emitter
	log   *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
}

// New builds a Manager. guard may be nil, in which case every exec is
// permitted and none is audited — acceptable only in tests.
func New(start Starter, emit guestagent.Emitter, guard Guard, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		start:    start,
		guard:    guard,
		emit:     emit,
		log:      log,
		sessions: make(map[string]*session),
	}
}

type session struct {
	id      string
	proc    Process
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	stdinMu sync.Mutex

	// written counts bytes streamed back, against maxOutput. The cap is held
	// per session because several execs run concurrently under different
	// authorisations.
	written   atomic.Int64
	maxOutput int64

	done chan struct{}
}

// Start authorises, launches, and begins relaying a process.
//
// It returns as soon as the process is running: the reply the host is waiting
// on carries the exec id and pid, and everything after that arrives as events.
// A process that fails to START is reported as an error here rather than as an
// exit event, because the two need different handling — only the second has an
// exit code to interpret.
func (m *Manager) Start(
	ctx context.Context,
	req guestwire.ExecRequest,
) (guestwire.ExecStartResponse, error) {
	if len(req.Argv) == 0 {
		return guestwire.ExecStartResponse{}, fmt.Errorf("%w: no program to run", ErrBadRequest)
	}
	// The host assigns the id — see guestwire.ExecRequest.ExecID for why. A
	// duplicate is refused rather than silently taking over an existing
	// session, which would cross two callers' streams.
	id := req.ExecID
	if id == "" {
		return guestwire.ExecStartResponse{}, fmt.Errorf("%w: no exec id", ErrBadRequest)
	}
	if m.session(id) != nil {
		return guestwire.ExecStartResponse{}, fmt.Errorf(
			"%w: exec %s is already running",
			ErrBadRequest,
			id,
		)
	}

	var limits Limits
	if m.guard != nil {
		var err error
		if limits, err = m.guard.Authorise(ctx, id, req); err != nil {
			return guestwire.ExecStartResponse{}, err
		}
	}

	// The process outlives this RPC, so it runs on its own context — cancelled
	// by Stop or when the process ends, never by the caller's reply deadline.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	proc, err := m.start.Start(runCtx, req)
	if err != nil {
		cancel()
		if m.guard != nil {
			m.guard.Finished(ctx, id, -1, err)
		}
		return guestwire.ExecStartResponse{}, fmt.Errorf(
			"guestexec: starting %q: %w",
			req.Argv[0],
			err,
		)
	}

	s := &session{
		id: id, proc: proc, cancel: cancel, stdin: proc.Stdin(),
		maxOutput: limits.MaxOutputBytes, done: make(chan struct{}),
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	go m.relay(runCtx, s)

	return guestwire.ExecStartResponse{ExecID: id, PID: proc.PID()}, nil
}

// relay pumps the process's output and publishes its exit.
func (m *Manager) relay(ctx context.Context, s *session) {
	defer close(s.done)

	var pumps sync.WaitGroup
	capErr := make(chan error, 2)

	pump := func(r io.Reader, kind string) {
		defer pumps.Done()
		w := guestagent.NewStreamWriter(m.emit, kind, s.id)
		err := m.copyCapped(ctx, w, r, s)
		if cerr := w.Close(ctx, err); cerr != nil {
			m.log.Warn("guestexec: closing stream", "exec", s.id, "kind", kind, "err", cerr)
		}
		if err != nil {
			// Stopping the relay is not enough: a process whose output we no
			// longer read fills its pipe and blocks in write() forever, so it
			// never exits, the other pump never sees EOF, and the exec hangs
			// instead of ending. Killing it is what makes the output cap
			// actually bound a runaway process.
			if kerr := s.proc.Signal(guestwire.SignalKill); kerr != nil {
				m.log.Warn("guestexec: killing a process whose stream failed",
					"exec", s.id, "err", kerr)
			}
			select {
			case capErr <- err:
			default: // both streams failed; the first reason is enough
			}
		}
	}

	if out := s.proc.Stdout(); out != nil {
		pumps.Add(1)
		go pump(out, guestwire.KindExecStdout)
	}
	if errStream := s.proc.Stderr(); errStream != nil {
		pumps.Add(1)
		go pump(errStream, guestwire.KindExecStderr)
	}

	// Wait for the output to drain BEFORE reporting the exit. A host that
	// receives the exit event first would be entitled to stop reading, and the
	// last of the process's output — usually the error message explaining the
	// exit code — would be discarded.
	pumps.Wait()

	code, signal, waitErr := s.proc.Wait()
	_ = s.proc.Close()

	// Everything after this point reports the exit, and it must NOT run on a
	// context this function is about to cancel. s.cancel() tears down the run
	// context; a Send made on it afterwards is refused before it reaches the
	// wire, so the audit record and the exit event are both silently dropped
	// and the host waits forever for an exit that was never sent.
	//
	// Found on a real guest, not in the loopback suite: the test transport
	// ignores the context it is handed, so both ends agreed perfectly about a
	// message that production never transmitted. It is the reason `weave exec`
	// printed a command's output and then hung.
	reportCtx := context.WithoutCancel(ctx)
	s.cancel()

	exit := guestwire.ExecExit{ExecID: s.id, Code: code, Signal: signal}
	select {
	case err := <-capErr:
		exit.Err = err.Error()
	default:
		if waitErr != nil {
			exit.Err = waitErr.Error()
		}
	}

	if m.guard != nil {
		var cause error
		if exit.Err != "" {
			cause = fmt.Errorf("%w: %s", ErrExecFailed, exit.Err)
		}
		m.guard.Finished(reportCtx, s.id, code, cause)
	}
	if err := m.emit.Emit(reportCtx, guestwire.KindExecExit, exit); err != nil {
		m.log.Warn("guestexec: emitting exit", "exec", s.id, "err", err)
	}

	m.mu.Lock()
	delete(m.sessions, s.id)
	m.mu.Unlock()
}

// copyCapped relays r into w, stopping if the session exceeds its output cap.
//
// The cap is on the SESSION, not per stream, so a process cannot double its
// allowance by splitting output across stdout and stderr.
func (m *Manager) copyCapped(
	ctx context.Context,
	w *guestagent.StreamWriter,
	r io.Reader,
	s *session,
) error {
	limit := s.maxOutput
	buf := make([]byte, guestwire.MaxChunkBytes)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if limit > 0 && s.written.Add(int64(n)) > limit {
				return fmt.Errorf("%w: the limit is %d bytes", ErrOutputLimit, limit)
			}
			if _, werr := w.WriteContext(ctx, buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			// A terminal master reports EIO when the child exits and the slave
			// side closes. That is the normal end of an interactive session,
			// not a failure — reporting it would put a spurious error on every
			// `weave ssh` that ended normally.
			if errors.Is(err, io.EOF) || isTerminalHangup(err) {
				return nil
			}
			return fmt.Errorf("guestexec: reading output: %w", err)
		}
	}
}

// Stdin writes a chunk of input to a running exec.
func (m *Manager) Stdin(chunk guestwire.Chunk) error {
	s := m.session(chunk.StreamID)
	if s == nil {
		return fmt.Errorf("%w: %s", ErrNoSuchExec, chunk.StreamID)
	}
	if s.stdin == nil {
		return fmt.Errorf("%w: %s", ErrNoStdin, chunk.StreamID)
	}

	// Serialised because the host may pipeline chunks: they arrive in order on
	// the channel but are dispatched on separate goroutines, and interleaved
	// writes would corrupt the process's input.
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()

	if len(chunk.Data) > 0 {
		if _, err := s.stdin.Write(chunk.Data); err != nil {
			return fmt.Errorf("guestexec: writing stdin: %w", err)
		}
	}
	if chunk.EOF {
		// Closing stdin is how a process that reads to EOF is told to proceed.
		// Without it, `cat` or `sort` waits forever.
		if err := s.stdin.Close(); err != nil {
			return fmt.Errorf("guestexec: closing stdin: %w", err)
		}
	}
	return nil
}

// Resize changes an interactive exec's window size.
func (m *Manager) Resize(req guestwire.ExecResizeRequest) error {
	s := m.session(req.ExecID)
	if s == nil {
		return fmt.Errorf("%w: %s", ErrNoSuchExec, req.ExecID)
	}
	return s.proc.Resize(req.Cols, req.Rows)
}

// Signal delivers a signal to a running exec — how a host cancels a process it
// started and no longer wants.
func (m *Manager) Signal(req guestwire.ExecSignalRequest) error {
	s := m.session(req.ExecID)
	if s == nil {
		return fmt.Errorf("%w: %s", ErrNoSuchExec, req.ExecID)
	}
	return s.proc.Signal(req.Signal)
}

// Stop terminates every running exec. The module calls it when it shuts down:
// processes started on behalf of a host that can no longer be reached have
// nobody to report to, and leaving them running leaks them into a guest whose
// agent has gone.
func (m *Manager) Stop() {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		_ = s.proc.Signal(guestwire.SignalKill)
		s.cancel()
	}
	for _, s := range sessions {
		<-s.done
	}
}

func (m *Manager) session(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// isTerminalHangup reports whether err is the read error a pseudo-terminal
// master returns once its child has exited, which is a normal end of stream.
func isTerminalHangup(err error) bool {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return errors.Is(err, errTerminalHangup)
}
