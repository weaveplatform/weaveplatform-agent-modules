// Package weaveclient is the HOST end of the capability channel: the side that
// issues commands, not the side that answers them.
//
// It is the counterpart of pkg/weavemodule. A CLI managing a VM from outside
// holds one Client per running guest, wired to the host end of that guest's
// hypervisor channel, and calls typed methods on it — Hello, Shutdown, Exec —
// which become weavewire commands. Each command is addressed to its
// capability (weave.presence, weave.exec, ...), never to an OS: the
// guest runs one module per capability, built for its own OS, and core routes
// by that address, so the same call works against a Linux, macOS or Windows
// guest. Framing comes from the sdk's hvchannel package, so the two ends cannot
// disagree about the bytes; the vocabulary comes from pkg/weavewire, so they
// cannot disagree about the meaning.
//
// # Transport
//
// The Client takes whatever byte stream reaches the guest: a VZ console pipe
// pair on macOS (see Pipes), an HvSocket net.Conn on Windows, a COM port. It
// owns that stream from New onward. Dial does the same through a Dialer.
//
// # One client per channel
//
// The channel has no resynchronisation (see hvchannel): a second reader on the
// same connection breaks the stream permanently rather than degrading it. A
// Client is therefore the single owner of the connection it is given — one read
// loop, writes serialised — and callers must not hand the same connection to
// two Clients. Concurrent CALLS are fine and expected: they are correlated by
// id, so a slow exec never blocks an urgent shutdown.
package weaveclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// ErrClosed is returned by a call made after the channel has ended.
var ErrClosed = errors.New("weaveclient: channel closed")

// EventHandler receives the raw payload of an unsolicited guest→host message
// (exec output, a file chunk, an exit notification). It runs on the read loop,
// so it must not block: hand work off rather than doing it here, or the whole
// channel stalls behind one handler.
type EventHandler func(kind string, data []byte)

// Client is a connection to one machine's weave capability modules.
type Client struct {
	log *slog.Logger

	conn io.Closer
	r    *bufio.Reader
	wmu  sync.Mutex // serialises writes; see the single-owner rule
	w    *bufio.Writer

	idPrefix string
	nextID   atomic.Uint64

	sessionTimeout time.Duration

	mu      sync.Mutex
	pending map[string]chan pendingReply
	events  map[string][]EventHandler
	closed  bool
	cause   error
	// execs routes exec events to their sessions. One set of handlers serves
	// the whole client and dispatches by exec id, rather than one per session
	// that would accumulate for the life of the connection.
	execs        map[string]*ExecSession
	execHandlers bool
	// downloads routes clipboard get streams by transfer id, the same way.
	downloads       map[string]*download
	downloadHandler bool

	// control carries channel-level frames (the authentication handshake) from
	// the read loop to whoever is waiting on them. Buffered so the loop never
	// blocks on a handshake nobody is running; awaitingControl says whether
	// anyone actually is.
	control         chan hvchannel.Envelope
	awaitingControl int

	done chan struct{}
}

// Options configures a Client.
type Options struct {
	// Log receives channel-level warnings. Defaults to slog.Default().
	Log *slog.Logger
	// SessionTimeout bounds how long a call to a per-user-console
	// capability (clipboard, display) waits for its answer before reporting
	// ErrNoSession. Zero means DefaultSessionTimeout; negative waits as long
	// as the call's own context allows.
	SessionTimeout time.Duration
}

// DefaultSessionTimeout is how long a per-user-console call waits for an
// answer by default. A module that is running answers these in milliseconds;
// the wait is for one that is not, and should end well before a person
// watching it gives up.
const DefaultSessionTimeout = 10 * time.Second

// ErrNoSession matches (via errors.Is) a call to a per-user-console capability
// that got no answer in time. Core runs those modules only while a user is
// logged in at the console and holds them in waiting-for-session otherwise,
// dropping what is sent to them — so silence, not a refusal, is what "nobody
// is logged in" looks like from here. The same silence comes from a machine
// with no module for the capability installed; Session tells the two apart.
//
// The error also matches context.DeadlineExceeded, for callers that already
// treat that as "no answer".
var ErrNoSession = errors.New("weaveclient: no answer from the console session")

// New wraps an already-open host-side channel connection and starts the read
// loop. The Client owns rwc from this point and closes it when the loop ends,
// when ctx ends, or on Close.
//
// The caller supplies the connection because the host end is created very
// differently per hypervisor — a pipe pair bridged into a VZ guest, a COM port,
// an HvSocket — and none of that belongs here.
func New(ctx context.Context, rwc io.ReadWriteCloser, opts Options) *Client {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	// A per-client random prefix keeps correlation ids unique across
	// reconnects: a reply to a command issued before a restart must not be
	// matched to a same-numbered command issued after it.
	var seed [6]byte
	_, _ = rand.Read(seed[:])

	c := &Client{
		log:            log,
		conn:           rwc,
		r:              bufio.NewReader(rwc),
		w:              bufio.NewWriter(rwc),
		idPrefix:       hex.EncodeToString(seed[:]),
		sessionTimeout: opts.SessionTimeout,
		pending:        make(map[string]chan pendingReply),
		events:         make(map[string][]EventHandler),
		control:        make(chan hvchannel.Envelope, 4),
		done:           make(chan struct{}),
	}
	if c.sessionTimeout == 0 {
		c.sessionTimeout = DefaultSessionTimeout
	}
	go c.readLoop()
	// The read loop blocks in Read, where a context cannot reach it. Closing
	// the connection is the only portable way to unblock a pipe or socket.
	go func() {
		select {
		case <-ctx.Done():
			c.shutdown(ctx.Err())
		case <-c.done:
		}
	}()
	return c
}

// On registers a handler for an event kind. Several handlers may share a kind;
// all are called, in registration order.
func (c *Client) On(kind string, h EventHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events[kind] = append(c.events[kind], h)
}

// Done is closed when the channel ends. Err reports why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the channel ended, or nil while it is live.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cause
}

// Close ends the channel and fails every call still waiting.
func (c *Client) Close() error {
	c.shutdown(ErrClosed)
	return nil
}

// Call issues one command and waits for its reply, decoding the response
// payload into out (which may be nil when the op returns nothing).
//
// It returns the guest's own error verbatim when the operation failed there, so
// a caller can tell "the guest refused" from "the channel broke" — the two need
// very different handling, and collapsing them is how a host ends up hard-
// stopping a VM that merely declined a request.
//
// A call to a per-user-console capability that goes unanswered ends after
// Options.SessionTimeout with an error matching ErrNoSession.
func (c *Client) Call(ctx context.Context, kind string, payload, out any) error {
	consoleCap, perUser := consoleCapability(kind)
	if !perUser {
		return c.call(ctx, kind, payload, out)
	}
	if c.sessionTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.sessionTimeout)
		defer cancel()
	}
	err := c.call(ctx, kind, payload, out)
	if errors.Is(err, context.DeadlineExceeded) {
		return noSession(kind, consoleCap)
	}
	return err
}

// consoleCapability reports whether kind belongs to a capability that runs in
// the console user's session.
func consoleCapability(kind string) (weavewire.Capability, bool) {
	c, ok := weavewire.CapabilityOf(kind)
	return c, ok && c.Placement() == weavewire.PlacementPerUserConsole
}

// noSession is the error for a per-user-console op that went unanswered —
// whether our deadline or the caller's ran out, the likely cause is the same.
func noSession(kind string, c weavewire.Capability) error {
	return fmt.Errorf(
		"%w: %s got no answer; core runs weave.%s only while a user is logged in at the console "+
			"(weave.session.current tells whether one is): %w",
		ErrNoSession, kind, c, context.DeadlineExceeded,
	)
}

func (c *Client) call(ctx context.Context, kind string, payload, out any) error {
	if weavewire.IsResult(kind) {
		return fmt.Errorf("%w: %q is a reply kind, not a command", ErrBadKind, kind)
	}
	address, ok := weavewire.AddressOf(kind)
	if !ok {
		return fmt.Errorf("%w: %q", ErrBadKind, kind)
	}

	id := c.idPrefix + "-" + strconv.FormatUint(c.nextID.Add(1), 10)
	data, err := weavewire.EncodeCommand(id, payload)
	if err != nil {
		return err
	}

	reply := make(chan pendingReply, 1)
	c.mu.Lock()
	if c.closed {
		err := c.cause
		c.mu.Unlock()
		return err
	}
	c.pending[id] = reply
	c.mu.Unlock()
	// Always retire the slot: an abandoned entry would leak, and worse, a late
	// reply would find a channel nobody is reading.
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(address, kind, data); err != nil {
		return err
	}

	select {
	case reply := <-reply:
		// A host-side failure is returned as itself, so a caller can match it
		// with errors.Is — "the guest refused this channel" and "the channel
		// closed" are conditions to branch on, not text to print.
		if reply.err != nil {
			return reply.err
		}
		res := reply.res
		if res.Err != "" {
			return &GuestError{Kind: kind, Msg: res.Err, Code: res.Code}
		}
		if out == nil || len(res.Payload) == 0 {
			return nil
		}
		if err := json.Unmarshal(res.Payload, out); err != nil {
			return fmt.Errorf("weaveclient: decoding %s reply: %w", kind, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

// Notify sends a command without waiting for a reply. It is for the host→guest
// half of a stream (exec stdin chunks), where a per-chunk round trip would
// serialise the stream to one chunk per latency period.
func (c *Client) Notify(kind string, payload any) error {
	address, ok := weavewire.AddressOf(kind)
	if !ok || weavewire.IsResult(kind) {
		return fmt.Errorf("%w: %q", ErrBadKind, kind)
	}
	data, err := weavewire.EncodeCommand("", payload)
	if err != nil {
		return err
	}
	return c.send(address, kind, data)
}

func (c *Client) send(address, kind string, data []byte) error {
	c.mu.Lock()
	if c.closed {
		err := c.cause
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrame(hvchannel.Envelope{Module: address, Kind: kind, Data: data})
}

// writeFrame writes and flushes one envelope. The caller holds wmu.
func (c *Client) writeFrame(env hvchannel.Envelope) error {
	if err := hvchannel.WriteEnvelope(c.w, env); err != nil {
		return fmt.Errorf("weaveclient: writing %s: %w", env.Kind, err)
	}
	if err := c.w.Flush(); err != nil {
		return fmt.Errorf("weaveclient: writing %s: %w", env.Kind, err)
	}
	return nil
}

// readLoop is the sole reader of the connection.
func (c *Client) readLoop() {
	for {
		env, err := hvchannel.ReadEnvelope(c.r)
		if err != nil {
			// A frame that will not decode costs one message, not the channel:
			// the length prefix kept the reader aligned. Only a stream failure
			// ends the loop.
			var syntax *json.SyntaxError
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &syntax) || errors.As(err, &typeErr) {
				c.log.Warn("weaveclient: undecodable frame", "err", err)
				continue
			}
			if errors.Is(err, io.EOF) {
				err = ErrClosed
			}
			c.shutdown(err)
			return
		}
		if env.Module == hvchannel.ControlModule {
			c.routeControl(env)
			continue
		}
		// Core stamps every module frame with the sender's address, so a frame
		// whose kind belongs to a different capability than its address is not
		// one this client can trust to mean what its kind says.
		if address, ok := weavewire.AddressOf(env.Kind); !ok || env.Module != address {
			c.log.Debug("weaveclient: frame from an unexpected address",
				"module", env.Module, "kind", env.Kind)
			continue
		}
		c.route(env)
	}
}

func (c *Client) route(env hvchannel.Envelope) {
	if !weavewire.IsResult(env.Kind) {
		c.mu.Lock()
		handlers := c.events[env.Kind]
		c.mu.Unlock()
		if len(handlers) == 0 {
			c.log.Debug("weaveclient: unhandled event", "kind", env.Kind)
			return
		}
		for _, h := range handlers {
			h(env.Kind, env.Data)
		}
		return
	}

	var res weavewire.Result
	if err := json.Unmarshal(env.Data, &res); err != nil {
		c.log.Warn("weaveclient: undecodable result", "kind", env.Kind, "err", err)
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[res.ID]
	c.mu.Unlock()
	if !ok {
		// A reply to a call that already gave up (timed out, or its context
		// was cancelled). Expected, not an error.
		c.log.Debug("weaveclient: reply with no waiter", "kind", env.Kind, "id", res.ID)
		return
	}
	ch <- pendingReply{res: res} // buffered; the waiter may have gone, but never blocks the loop
}

// shutdown ends the channel once, recording why and releasing every waiter.
func (c *Client) shutdown(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.cause = cause
	pending := c.pending
	c.pending = make(map[string]chan pendingReply)
	c.mu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- pendingReply{err: cause}:
		default:
		}
	}
	close(c.done)
	_ = c.conn.Close()
}

// pendingReply is what a waiting call receives: either the guest's own result,
// or a host-side error that ended the wait before one arrived. The two are
// separate fields because they mean different things to a caller — the guest
// declining an operation is not the same as never having been asked.
type pendingReply struct {
	res weavewire.Result
	err error
}

// GuestError is an operation the guest refused or failed, as opposed to a
// channel or encoding failure on the host side.
type GuestError struct {
	Kind string
	Msg  string
	// Code is the guest's classification of the failure, when it gave one
	// (weavewire.CodeUnsupported).
	Code string
}

func (e *GuestError) Error() string { return "weave: " + e.Kind + ": " + e.Msg }

// Is lets errors.Is(err, ErrUnsupported) match a guest that answered an op
// with weavewire.CodeUnsupported.
func (e *GuestError) Is(target error) bool {
	return target == ErrUnsupported && e.Code == weavewire.CodeUnsupported
}

// ErrUnsupported matches (via errors.Is) an op the guest's OS cannot perform.
// The module is healthy; the host should feature-gate rather than retry.
var ErrUnsupported = errors.New("weaveclient: operation not supported by this guest")

// ErrBadKind reports a kind the client cannot address: a reply kind, or one
// not of the form weave.<capability>.<op>.
var ErrBadKind = errors.New("weaveclient: not an addressable command kind")
