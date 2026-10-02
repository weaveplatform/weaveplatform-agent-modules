package guesthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// ErrExecFailed reports that the guest could not run a process to completion:
// a policy refusal mid-flight, an I/O failure, output over its cap. A process
// that ran and exited non-zero is not this; its code comes back from Wait.
var ErrExecFailed = errors.New("guestweave: exec failed")

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
func (c *Client) Exec(ctx context.Context, req guestwire.ExecRequest) (*ExecSession, error) {
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
	if err := c.Call(ctx, guestwire.KindExecStart, req, &s.started); err != nil {
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
			guestwire.KindExecStdout,
			c.routeChunk(func(s *ExecSession) *streamPipe { return &s.stdout }),
		)
		c.On(
			guestwire.KindExecStderr,
			c.routeChunk(func(s *ExecSession) *streamPipe { return &s.stderr }),
		)
		c.On(guestwire.KindExecExit, c.routeExit)
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
		var chunk guestwire.Chunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			c.log.Warn("guesthost: undecodable exec chunk", "err", err)
			return
		}
		if s := c.execSession(chunk.StreamID); s != nil {
			pick(s).accept(chunk)
		}
	}
}

func (c *Client) routeExit(_ string, data []byte) {
	var exit guestwire.ExecExit
	if err := json.Unmarshal(data, &exit); err != nil {
		c.log.Warn("guesthost: undecodable exec exit", "err", err)
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
	started guestwire.ExecStartResponse

	stdout, stderr streamPipe

	stdinMu  sync.Mutex
	stdinSeq uint64

	once sync.Once
	done chan struct{}
	exit guestwire.ExecExit
}

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
// set, or the guest has no pipe to write into.
//
// Input is sent without waiting for acknowledgement — a reply per chunk would
// reduce the stream to one chunk per round trip. Sequence numbers let the guest
// detect a gap; a write failure surfaces on the exit event.
func (s *ExecSession) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), guestwire.MaxChunkBytes)
		if err := s.sendStdin(guestwire.Chunk{Data: p[:n]}); err != nil {
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
	return s.sendStdin(guestwire.Chunk{EOF: true})
}

// sendStdin stamps and sends one input chunk. The lock covers the sequence
// number AND the send, because a number assigned under a lock and then sent
// outside it can still reach the guest out of order.
func (s *ExecSession) sendStdin(chunk guestwire.Chunk) error {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	chunk.StreamID = s.id
	chunk.Seq = s.stdinSeq
	s.stdinSeq++
	return s.client.Notify(guestwire.KindExecStdin, chunk)
}

var _ io.Writer = (*ExecSession)(nil)

// Resize changes the terminal size, for the host's SIGWINCH.
func (s *ExecSession) Resize(ctx context.Context, cols, rows uint16) error {
	return s.client.Call(ctx, guestwire.KindExecResize, guestwire.ExecResizeRequest{
		ExecID: s.id, Cols: cols, Rows: rows,
	}, nil)
}

// Signal delivers a portable signal — how a caller cancels a process it started
// and no longer wants.
func (s *ExecSession) Signal(ctx context.Context, name string) error {
	return s.client.Call(ctx, guestwire.KindExecSignal, guestwire.ExecSignalRequest{
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

func (s *ExecSession) finish(exit guestwire.ExecExit) {
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
// end-of-stream rules through guestwire.StreamAssembler.
type streamPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	asm    guestwire.StreamAssembler
	buf    []byte
	closed bool
	err    error
}

func (p *streamPipe) init() { p.cond = sync.NewCond(&p.mu) }

func (p *streamPipe) accept(chunk guestwire.Chunk) {
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
