package weaveagent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// ErrResultKind reports an attempt to emit a reply kind as an event.
var ErrResultKind = errors.New("weaveagent: result kinds are replies, not events")

// Emitter sends a message the host did not ask for: exec stdout as it is
// produced, an exit notification, a chunk of a file the host is pulling.
//
// It is separate from the Dispatcher's reply path on purpose. A reply carries a
// correlation id and answers exactly one command; an event carries none and may
// arrive any number of times, or never. Routing on the host stays unambiguous
// because the host is always the commander: a guest→host message whose kind
// ends in ".result" is a reply to correlate, and anything else is an event to
// dispatch by kind.
type Emitter interface {
	Emit(ctx context.Context, kind string, payload any) error
}

// emitter implements Emitter over the same Sender the Dispatcher replies on.
type emitter struct{ send Sender }

// NewEmitter builds an Emitter over send. Backends that produce streams take
// one of these; backends that only answer commands do not need it.
func NewEmitter(send Sender) Emitter { return &emitter{send: send} }

// Emitter returns an Emitter sharing this dispatcher's transport, so a backend
// registered here can push events without being handed a second dependency.
func (d *Dispatcher) Emitter() Emitter { return NewEmitter(d.send) }

// Emit sends one event. It refuses result kinds: a reply must go through the
// dispatcher so it carries the correlation id the caller is blocked on, and an
// event spelled like a reply would be matched against a command that never
// existed.
func (e *emitter) Emit(ctx context.Context, kind string, payload any) error {
	if weavewire.IsResult(kind) {
		return fmt.Errorf(
			"%w: %q — replies go through the dispatcher, not Emit",
			ErrResultKind,
			kind,
		)
	}
	data, err := weavewire.EncodeEvent(payload)
	if err != nil {
		return err
	}
	if _, err := e.send.Send(ctx, modulesdk.Message{
		Peer: modulesdk.PeerHypervisor, Kind: kind, Data: data,
	}, false); err != nil {
		return fmt.Errorf("weaveagent: emitting %s: %w", kind, err)
	}
	return nil
}

// StreamWriter turns bytes into ordered weavewire.Chunk events on one kind.
// Exec stdio and file-get both write through it, so the sequencing and
// end-of-stream rules exist once.
//
// It is safe for one producer per stream. Two goroutines writing the same
// StreamWriter would interleave chunk bodies, which the sequence numbers would
// then certify as intact — so the mutex here guards the seq/body pairing, not
// merely the counter.
type StreamWriter struct {
	emit     Emitter
	kind     string
	streamID string

	mu     sync.Mutex
	seq    uint64
	closed bool
}

// NewStreamWriter builds a writer that emits chunks of streamID on kind.
func NewStreamWriter(emit Emitter, kind, streamID string) *StreamWriter {
	return &StreamWriter{emit: emit, kind: kind, streamID: streamID}
}

// Write emits p as one or more chunks, splitting at weavewire.MaxChunkBytes.
// It satisfies io.Writer so a process's stdout can be copied straight into it.
func (w *StreamWriter) Write(p []byte) (int, error) {
	return w.WriteContext(context.Background(), p)
}

// WriteContext is Write with a cancellable context — preferred where the caller
// has one, so a stopped module does not keep pumping a dead stream.
func (w *StreamWriter) WriteContext(ctx context.Context, p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), weavewire.MaxChunkBytes)
		if err := w.emitChunk(ctx, weavewire.Chunk{Data: p[:n]}); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// Close ends the stream with an EOF chunk, carrying cause when the producer
// failed partway. It is idempotent: a stream must be terminated exactly once,
// and a double Close (deferred Close plus an explicit error path) is the
// obvious way to send two EOFs for one stream.
func (w *StreamWriter) Close(ctx context.Context, cause error) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	chunk := weavewire.Chunk{EOF: true}
	if cause != nil {
		chunk.Err = cause.Error()
	}
	return w.emitChunk(ctx, chunk)
}

func (w *StreamWriter) emitChunk(ctx context.Context, c weavewire.Chunk) error {
	w.mu.Lock()
	c.StreamID = w.streamID
	c.Seq = w.seq
	w.seq++
	w.mu.Unlock()
	return w.emit.Emit(ctx, w.kind, c)
}
