// Package guestagent is the dispatch layer under every guestweave capability
// module: it reads inbound commands from core's hypervisor transport, routes
// each by kind to a registered handler, and sends the reply back over the same
// channel, on core's fire-and-forget Message primitive with a correlation id
// rather than a synchronous request/response socket.
//
// Modules do not use it directly: pkg/guestmodule builds one Dispatcher per
// module and lets the capability's Service register its handlers on it.
package guestagent

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// Handler runs one operation: it receives the decoded per-op request payload
// and returns the per-op response payload, or an error that becomes Result.Err.
type Handler func(ctx context.Context, payload []byte) (response []byte, err error)

// DeferredHandler is a Handler that also returns work to run AFTER its reply is
// on the wire.
//
// Power is why this exists. The command that shuts a guest down terminates the
// agent along with the rest of the OS, so a reply written afterwards races the
// process being killed and the host sees a dead channel instead of an answer.
// The ordering — reply first, act second — is the entire contract, and it is
// enforced here rather than trusted to each OS backend.
//
// The deferred work runs only if the reply was actually sent. If it was not,
// the host does not know the request was accepted, and powering the guest off
// anyway would take down a VM whose owner believes the request failed.
type DeferredHandler func(ctx context.Context, payload []byte) (response []byte, after func(), err error)

// Sender is the subset of modulesdk.Transport the dispatcher needs to reply.
// modulesdk.Host.Transport() satisfies it.
type Sender interface {
	Send(ctx context.Context, msg modulesdk.Message, queueOffline bool) (delivered bool, err error)
}

// Dispatcher routes inbound hypervisor commands to handlers and replies.
type Dispatcher struct {
	log  *slog.Logger
	send Sender

	mu       sync.RWMutex
	handlers map[string]DeferredHandler
}

// New builds a Dispatcher that replies over send.
func New(send Sender, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{log: log, send: send, handlers: make(map[string]DeferredHandler)}
}

// Handle registers h for a command kind (a guestwire.Kind* constant). It
// panics on a duplicate or on a kind that is itself a reply kind — both are
// programmer errors caught at wiring time.
func (d *Dispatcher) Handle(kind string, h Handler) {
	if guestwire.IsResult(kind) {
		panic("guestagent: cannot register a handler for a result kind: " + kind)
	}
	d.handle(kind, func(ctx context.Context, payload []byte) ([]byte, func(), error) {
		resp, err := h(ctx, payload)
		return resp, nil, err
	})
}

// HandleDeferred registers a handler whose work happens after its reply is
// sent. See DeferredHandler.
func (d *Dispatcher) HandleDeferred(kind string, h DeferredHandler) {
	if guestwire.IsResult(kind) {
		panic("guestagent: cannot register a handler for a result kind: " + kind)
	}
	d.handle(kind, h)
}

func (d *Dispatcher) handle(kind string, h DeferredHandler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.handlers[kind]; dup {
		panic("guestagent: duplicate handler for kind: " + kind)
	}
	d.handlers[kind] = h
}

// Kinds returns the registered command kinds, sorted — for the parity test.
func (d *Dispatcher) Kinds() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.handlers))
	for k := range d.handlers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Run consumes inbound messages until in closes (core closes it on ctx end or
// stream failure).
//
// Most commands are dispatched on their own goroutine, so a slow operation (a
// long exec) never blocks an urgent one (power); replies carry the correlation
// id, so out-of-order completion is fine.
//
// The exception is a stream's host→guest half. Those messages are only
// meaningful in the order the host sent them — stdin chunks against each other,
// and every chunk against the EOF that closes the pipe — and the channel
// already delivers them in that order. A goroutine per message throws it away:
// the EOF wins the race, the pipe closes, and the chunks that follow fail with
// "file already closed". Observed on a real guest, where `weave exec -i`
// delivered nothing at all to the process and reported success; the loopback
// suite had the same race but almost always won it.
//
// So ordered kinds go to ONE serial worker. Not one per stream: a single queue
// is enough to preserve what matters (order within a stream), needs no
// bookkeeping to know when a stream ends, and cannot leak a goroutine per exec.
// The cost is that a blocked stdin write for one exec delays stdin for another,
// which is a far better failure than delaying a shutdown.
func (d *Dispatcher) Run(ctx context.Context, in <-chan modulesdk.Message) {
	var wg sync.WaitGroup

	ordered := make(chan modulesdk.Message, orderedQueueDepth)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for m := range ordered {
			d.dispatch(ctx, m)
		}
	}()

	for msg := range in {
		if guestwire.IsResult(msg.Kind) {
			continue // our own echoes; we are the responder, not the caller
		}
		if guestwire.IsOrderedInbound(msg.Kind) {
			ordered <- msg
			continue
		}
		wg.Add(1)
		go func(m modulesdk.Message) {
			defer wg.Done()
			d.dispatch(ctx, m)
		}(msg)
	}
	close(ordered)
	wg.Wait()
}

// orderedQueueDepth buys enough slack that a burst of stdin chunks does not
// stall the read loop behind one slow write, without letting an unread pipe
// buffer without limit.
const orderedQueueDepth = 64

func (d *Dispatcher) dispatch(ctx context.Context, msg modulesdk.Message) {
	var cmd guestwire.Command
	if err := json.Unmarshal(msg.Data, &cmd); err != nil {
		d.log.Warn("guestweave: undecodable command", "kind", msg.Kind, "err", err)
		return // no id to correlate a reply to
	}

	d.mu.RLock()
	h, ok := d.handlers[msg.Kind]
	d.mu.RUnlock()

	var (
		resp  []byte
		after func()
		opErr error
	)
	if !ok {
		opErr = errUnknownKind(msg.Kind)
	} else {
		resp, after, opErr = h(ctx, cmd.Payload)
	}

	// A command sent with no correlation id wants no reply. That is how a
	// stream's host→guest half stays a stream: replying per stdin chunk would
	// serialise it to one chunk per round trip. There is nobody to tell about a
	// failure either, so it is logged here and surfaces on the stream's own
	// terminating event.
	if cmd.ID == "" {
		if opErr != nil {
			d.log.Warn("guestweave: unacknowledged command failed", "kind", msg.Kind, "err", opErr)
		}
		if after != nil {
			after()
		}
		return
	}

	var respPayload any
	if len(resp) > 0 {
		respPayload = json.RawMessage(resp)
	}
	data, err := guestwire.EncodeResult(cmd.ID, respPayload, opErr)
	if err != nil {
		d.log.Error("guestweave: encoding result", "kind", msg.Kind, "err", err)
		return
	}
	reply := modulesdk.Message{
		Peer: modulesdk.PeerHypervisor,
		Kind: guestwire.ResultKind(msg.Kind),
		Data: data,
	}
	if _, err := d.send.Send(ctx, reply, false); err != nil {
		// The reply never left. Deferred work is abandoned deliberately: the
		// host does not know its request was accepted, and the canonical
		// deferred action is a shutdown — taking a VM down while its owner
		// believes the request failed is worse than not acting at all.
		d.log.Warn("guestweave: sending result", "kind", reply.Kind, "err", err)
		return
	}
	if after != nil {
		after()
	}
}

type unknownKindError string

func (e unknownKindError) Error() string { return "unknown command kind: " + string(e) }

func errUnknownKind(kind string) error { return unknownKindError(kind) }
