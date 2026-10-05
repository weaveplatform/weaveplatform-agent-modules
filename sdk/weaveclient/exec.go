package weaveclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// ErrExecFailed reports that the guest could not run a process to completion:
// a policy refusal mid-flight, an I/O failure, output over its cap. A process
// that ran and exited non-zero is not this; its code comes back from Wait.
var ErrExecFailed = errors.New("weave: exec failed")

// Exec runs a process inside the guest and returns a handle to it.
//
// The handle is the point: a caller writes to Stdin, reads Stdout and Stderr,
// and blocks on Wait — the same shape as os/exec — while underneath it is chunk
// events multiplexed with every other exec over one channel. The CLI's `exec`
// and `ssh` verbs are meant to be thin wrappers over this.
//
// Exec returns once the process has STARTED. A process that fails to start
// comes back as an error here; one that starts and then fails reports through
// Wait. The distinction matters because only the second has an exit code.
func (c *Client) Exec(ctx context.Context, req weavewire.ExecRequest) (*ExecSession, error) {
	// The id is minted HERE and registered BEFORE the request goes out. The
	// guest starts emitting output the moment the process runs, and its reply
	// and those events share one ordered channel — so a host that waited for
	// the reply to learn the id would race its own read loop and drop the
	// opening chunks of every fast command.
	req.ExecID = c.idPrefix + "-exec-" + strconv.FormatUint(c.nextID.Add(1), 10)

	s := &ExecSession{client: c, id: req.ExecID, done: make(chan struct{})}
	s.stdout.init()
	s.stderr.init()

	c.registerExec(s)
	if err := c.Call(ctx, weavewire.KindExecStart, req, &s.started); err != nil {
		c.unregisterExec(s.id)
		return nil, err
	}
	return s, nil
}

// registerExec adds a session to the client's exec table, installing the shared
// event handlers on first use.
//
// One handler per kind for the whole client, dispatching by exec id — rather
// than one handler per session, which would leave every finished exec's handler
// on the client for the life of the connection and run all of them for every
// chunk.
func (c *Client) registerExec(s *ExecSession) {
	c.mu.Lock()
	if c.execs == nil {
		c.execs = make(map[string]*ExecSession)
	}
	c.execs[s.id] = s
	install := !c.execHandlers
	c.execHandlers = true
	c.mu.Unlock()

	if install {
		c.On(
			weavewire.KindExecStdout,
			c.routeChunk(func(s *ExecSession) *streamPipe { return &s.stdout }),
		)
		c.On(
			weavewire.KindExecStderr,
			c.routeChunk(func(s *ExecSession) *streamPipe { return &s.stderr }),
		)
		c.On(weavewire.KindExecExit, c.routeExit)
	}
}

func (c *Client) unregisterExec(id string) {
	c.mu.Lock()
	delete(c.execs, id)
	c.mu.Unlock()
}

func (c *Client) execSession(id string) *ExecSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.execs[id]
}

func (c *Client) routeChunk(pick func(*ExecSession) *streamPipe) EventHandler {
	return func(_ string, data []byte) {
		var chunk weavewire.Chunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			c.log.Warn("weaveclient: undecodable exec chunk", "err", err)
			return
		}
		if s := c.execSession(chunk.StreamID); s != nil {
			pick(s).accept(chunk)
		}
	}
}

func (c *Client) routeExit(_ string, data []byte) {
	var exit weavewire.ExecExit
	if err := json.Unmarshal(data, &exit); err != nil {
		c.log.Warn("weaveclient: undecodable exec exit", "err", err)
		return
	}
	if s := c.execSession(exit.ExecID); s != nil {
		s.finish(exit)
		c.unregisterExec(exit.ExecID)
	}
}

// ExecSession is a running process in the guest.
type ExecSession struct {
	client  *Client
	id      string
	started weavewire.ExecStartResponse

	stdout, stderr streamPipe

	stdinMu  sync.Mutex
	stdinSeq uint64
	// unconfirmed is set once a chunk has gone out without a fence, while
	// the client did not yet know core reports refusals.
	unconfirmed bool

	// failMu guards the fields core's delivery.failed for this session sets
	// from the read loop. Its own lock, not stdinMu: the read loop sets them
	// while a writer holds stdinMu waiting for exactly that answer.
	failMu sync.Mutex
	// failed ends the input for good: the module is gone or not running, or
	// a chunk was refused that cannot be resent in order.
	failed error
	// confirming is set while one input chunk is out and its fence is not
	// back; refused is core's busy for that chunk, if it came.
	confirming bool
	refused    *DeliveryError

	once sync.Once
	done chan struct{}
	exit weavewire.ExecExit
}

// Backoff between attempts at an input chunk core refused as busy. The floor
// is about one dispatch of the module's queue; the ceiling keeps a process
// that has stopped reading from costing more than a few frames a second.
const (
	stdinRetryFloor   = 2 * time.Millisecond
	stdinRetryCeiling = 250 * time.Millisecond
)

// ID is the identifier every message about this exec carries.
func (s *ExecSession) ID() string { return s.id }

// PID is the process id inside the guest, for an operator looking at it there.
func (s *ExecSession) PID() int { return s.started.PID }

// Stdout is the process's output. Under a pseudo-terminal it carries
// everything, because a terminal merges the two streams.
func (s *ExecSession) Stdout() io.Reader { return &s.stdout }

// Stderr is the process's error output; empty for a terminal exec.
func (s *ExecSession) Stderr() io.Reader { return &s.stderr }

// Write sends input to the process. The exec must have been started with Stdin
// set, or the guest has no pipe to write into. It is WriteContext without a
// deadline: it still returns when the process exits or the channel ends.
func (s *ExecSession) Write(p []byte) (int, error) {
	return s.WriteContext(context.Background(), p)
}

// WriteContext sends input to the process, in chunks of at most
// weavewire.MaxChunkBytes, returning once core has taken each one.
//
// Against a core that reports undeliverable frames (weave-agent v0.9.2 and
// later, which the client knows once core has shown it has a registry — see
// ErrRegistryTimeout) each chunk is confirmed before the next goes out, and a
// chunk core refuses because the module's queue is full (ErrModuleBusy) is
// sent again with backoff until it is taken. Busy is back-pressure from a
// process reading slower than the host writes, not a failure, so it never
// ends the input. What ends it is the module being gone or not running —
// which ends the session too — the process exiting, the channel ending, or
// ctx.
//
// Against an older core, or before the client knows which it has, chunks go
// out without waiting, as that core never says it dropped one.
func (s *ExecSession) WriteContext(ctx context.Context, p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), weavewire.MaxChunkBytes)
		if err := s.sendStdin(ctx, weavewire.Chunk{Data: p[:n]}); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// CloseStdin tells the process no more input is coming. Without it a command
// that reads to EOF — cat, sort, a shell reading a script — waits forever.
func (s *ExecSession) CloseStdin() error {
	return s.CloseStdinContext(context.Background())
}

// CloseStdinContext is CloseStdin bounded by ctx. The end of input is a chunk
// like any other and is retried past a busy module the same way, behind all
// the input before it.
func (s *ExecSession) CloseStdinContext(ctx context.Context) error {
	return s.sendStdin(ctx, weavewire.Chunk{EOF: true})
}

// sendStdin stamps and sends one input chunk. The lock covers the sequence
// number AND the send, because a number assigned under a lock and then sent
// outside it can still reach the guest out of order.
//
// The envelope carries the exec id, so a chunk core cannot deliver comes back
// to this session as a delivery.failed rather than vanishing.
func (s *ExecSession) sendStdin(ctx context.Context, chunk weavewire.Chunk) error {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if err := s.failure(); err != nil {
		return err
	}
	chunk.StreamID = s.id
	chunk.Seq = s.stdinSeq

	confirm, err := s.client.reportsUndeliverable(ctx)
	if err != nil {
		return err
	}
	if !confirm {
		if err := s.client.notify(weavewire.KindExecStdin, s.id, chunk); err != nil {
			return err
		}
		s.stdinSeq++
		s.unconfirmed = true
		return nil
	}
	if s.unconfirmed {
		// The client has learnt core reports refusals since this session
		// last sent. A refusal of an earlier, unconfirmed chunk may still be
		// on its way, and must not be taken for this chunk's: one fence with
		// nothing being confirmed lets it land where it belongs (undeliverable).
		if _, err := s.client.listModules(ctx, 0); err != nil {
			return fmt.Errorf("weaveclient: confirming input for exec %s: %w", s.id, err)
		}
		s.unconfirmed = false
		if err := s.failure(); err != nil {
			return err
		}
	}
	return s.sendConfirmed(ctx, chunk)
}

// sendConfirmed sends chunk and waits until core has either taken it or
// refused it, resending it with backoff while the refusal is busy.
//
// Core has no acknowledgement for input, and does not need one: it handles
// the channel's frames one at a time, in order, and writes its delivery.failed
// for a frame before it reads the next. So a modules.list sent behind the
// chunk is a fence — by the time its answer arrives, any refusal of the chunk
// has arrived ahead of it, and no refusal means the module's queue took it.
//
// One chunk at a time, rather than a window of them, is what keeps the input
// in order. With several out, core can refuse one and take the next once the
// module drains a slot, and the guest writes them to the process in the order
// they arrived — the busy chunk, resent, would land after its successor.
// The fence costs a round trip per chunk of up to 32 KiB, and it is also the
// pacing: a host can never have more of a session's input in core's hands
// than the one chunk.
func (s *ExecSession) sendConfirmed(ctx context.Context, chunk weavewire.Chunk) error {
	backoff := stdinRetryFloor
	for {
		s.setConfirming()
		if err := s.client.notify(weavewire.KindExecStdin, s.id, chunk); err != nil {
			_ = s.endConfirming() // the send failed; there is nothing to confirm
			return err
		}
		_, ferr := s.client.listModules(ctx, 0)
		refused := s.endConfirming()
		// A module that is gone or not running ended the session before the
		// fence's answer arrived; that, not the fence, is why input stops.
		if err := s.failure(); err != nil {
			return err
		}
		if ferr != nil {
			return fmt.Errorf("weaveclient: confirming input for exec %s: %w", s.id, ferr)
		}
		if refused == nil {
			s.stdinSeq++
			return nil
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf(
				"%w (exec %s input chunk %d): %w",
				ctx.Err(),
				s.id,
				chunk.Seq,
				refused,
			)
		case <-s.done:
			timer.Stop()
			return fmt.Errorf(
				"weaveclient: exec %s ended before its input was taken: %w", s.id, refused,
			)
		case <-s.client.done:
			timer.Stop()
			return s.client.Err()
		}
		backoff = min(backoff*2, stdinRetryCeiling)
	}
}

func (s *ExecSession) setConfirming() {
	s.failMu.Lock()
	s.confirming, s.refused = true, nil
	s.failMu.Unlock()
}

func (s *ExecSession) endConfirming() *DeliveryError {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	refused := s.refused
	s.confirming, s.refused = false, nil
	return refused
}

func (s *ExecSession) failure() error {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	return s.failed
}

// undeliverable records core's report that a frame for this session never
// reached the exec module.
//
// Busy is the module's queue being full. For a chunk being confirmed it is
// noted, and the chunk is sent again. For a chunk that went out unconfirmed —
// sent before the client knew core reports refusals — which chunk it was, and
// whether later ones overtook it, cannot be known, so the input stops rather
// than reach the process with a hole in it; the process runs on.
//
// A module that is gone or not running will never report the exit either,
// so the session ends here with the error — Wait returns it and the output
// readers end with it.
func (s *ExecSession) undeliverable(err *DeliveryError) {
	s.failMu.Lock()
	if err.Reason == hvchannel.ReasonBusy {
		if s.confirming {
			s.refused = err
		} else if s.failed == nil {
			s.failed = fmt.Errorf(
				"weaveclient: exec %s input lost before core was known to report refusals: %w",
				s.id, err,
			)
		}
		s.failMu.Unlock()
		return
	}
	if s.failed == nil {
		s.failed = err
	}
	s.failMu.Unlock()
	s.once.Do(func() {
		s.exit = weavewire.ExecExit{ExecID: s.id, Code: -1}
		s.stdout.fail(err)
		s.stderr.fail(err)
		close(s.done)
	})
	s.client.unregisterExec(s.id)
}

var _ io.Writer = (*ExecSession)(nil)

// Resize changes the terminal size, for the host's SIGWINCH.
func (s *ExecSession) Resize(ctx context.Context, cols, rows uint16) error {
	return s.client.Call(ctx, weavewire.KindExecResize, weavewire.ExecResizeRequest{
		ExecID: s.id, Cols: cols, Rows: rows,
	}, nil)
}

// Signal delivers a portable signal — how a caller cancels a process it started
// and no longer wants.
func (s *ExecSession) Signal(ctx context.Context, name string) error {
	return s.client.Call(ctx, weavewire.KindExecSignal, weavewire.ExecSignalRequest{
		ExecID: s.id, Signal: name,
	}, nil)
}

// Wait blocks until the process exits, returning its exit code.
//
// A non-zero code is NOT an error. A command that legitimately reports failure
// — grep finding nothing, a test failing — has run correctly, and conflating
// that with "the exec broke" is how a caller ends up retrying a working
// command. An error is returned only when the guest could not run the process
// to completion, or the channel died first.
func (s *ExecSession) Wait(ctx context.Context) (int, error) {
	select {
	case <-s.done:
		if err := s.failure(); err != nil && !errors.Is(err, ErrModuleBusy) {
			return -1, err
		}
		if s.exit.Err != "" {
			return s.exit.Code, fmt.Errorf("%w: exec %s: %s", ErrExecFailed, s.id, s.exit.Err)
		}
		return s.exit.Code, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	case <-s.client.Done():
		return -1, s.client.Err()
	}
}

// ExitSignal names the signal that killed the process, if one did. Meaningful
// only after Wait returns.
func (s *ExecSession) ExitSignal() string { return s.exit.Signal }

func (s *ExecSession) finish(exit weavewire.ExecExit) {
	s.once.Do(func() {
		s.exit = exit
		// Both streams close on exit. The guest drains output BEFORE reporting
		// the exit, so anything undelivered by now is not coming, and a reader
		// left blocking would hang a caller whose process has already finished.
		s.stdout.close()
		s.stderr.close()
		close(s.done)
	})
}

// streamPipe turns ordered Chunks into an io.Reader, enforcing the gap and
// end-of-stream rules through weavewire.StreamAssembler.
type streamPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	asm    weavewire.StreamAssembler
	buf    []byte
	closed bool
	err    error
}

func (p *streamPipe) init() { p.cond = sync.NewCond(&p.mu) }

func (p *streamPipe) accept(chunk weavewire.Chunk) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	data, err := p.asm.Accept(chunk)
	if err != nil {
		// A gap is fatal to the stream by design: bytes after it cannot be
		// trusted, and continuing would hand the caller output with a hole in
		// it that reads as complete.
		p.err = err
		p.closed = true
		p.cond.Broadcast()
		return
	}
	p.buf = append(p.buf, data...)
	if p.asm.Done() {
		p.err = p.asm.Err()
		p.closed = true
	}
	p.cond.Broadcast()
}

func (p *streamPipe) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
}

// fail ends the stream with err, after whatever is already buffered.
func (p *streamPipe) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && p.err == nil {
		p.err = err
	}
	p.closed = true
	p.cond.Broadcast()
}

// Read drains buffered output, blocking until there is some or the stream ends.
// Buffered bytes are always returned before the stream's error, so output
// produced before a failure is not lost to it.
func (p *streamPipe) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 && !p.closed {
		p.cond.Wait()
	}
	if len(p.buf) > 0 {
		n := copy(dst, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}
	if p.err != nil {
		return 0, p.err
	}
	return 0, io.EOF
}
