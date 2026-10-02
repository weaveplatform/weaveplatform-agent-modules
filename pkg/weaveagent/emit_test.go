package weaveagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// recordingSender captures what a backend emitted, in order.
type recordingSender struct{ sent []modulesdk.Message }

func (r *recordingSender) Send(_ context.Context, msg modulesdk.Message, _ bool) (bool, error) {
	r.sent = append(r.sent, msg)
	return true, nil
}

func (r *recordingSender) chunks(t *testing.T) []weavewire.Chunk {
	t.Helper()
	out := make([]weavewire.Chunk, 0, len(r.sent))
	for _, m := range r.sent {
		var c weavewire.Chunk
		if err := json.Unmarshal(m.Data, &c); err != nil {
			t.Fatalf("decoding emitted chunk: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// A payload larger than the cap must arrive as several chunks that reassemble
// exactly — the split is the mechanism exec stdio and file transfer both rely
// on, so it has to be lossless at the boundary.
func TestStreamWriterSplitsAtTheCapAndReassembles(t *testing.T) {
	rec := &recordingSender{}
	w := NewStreamWriter(NewEmitter(rec), "weave.exec.stdout", "exec-1")

	payload := bytes.Repeat([]byte("x"), weavewire.MaxChunkBytes*2+7)
	n, err := w.Write(payload)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(payload) {
		t.Fatalf("wrote %d of %d bytes", n, len(payload))
	}
	if err := w.Close(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	chunks := rec.chunks(t)
	if len(chunks) != 4 { // 2 full + 1 remainder + EOF
		t.Fatalf("got %d chunks, want 4", len(chunks))
	}

	var asm weavewire.StreamAssembler
	var got bytes.Buffer
	for _, c := range chunks {
		if c.StreamID != "exec-1" {
			t.Fatalf("chunk stream id = %q", c.StreamID)
		}
		data, err := asm.Accept(c)
		if err != nil {
			t.Fatalf("the writer produced a stream its own assembler rejects: %v", err)
		}
		got.Write(data)
	}
	if !asm.Done() {
		t.Fatal("Close did not terminate the stream")
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("reassembled %d bytes, want %d", got.Len(), len(payload))
	}
}

// Close is deferred in every realistic caller and also called explicitly on the
// error path, so a second Close must not put a second EOF on the wire.
func TestStreamWriterCloseIsIdempotent(t *testing.T) {
	rec := &recordingSender{}
	w := NewStreamWriter(NewEmitter(rec), "weave.exec.stdout", "exec-1")
	ctx := context.Background()

	if err := w.Close(ctx, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}

	chunks := rec.chunks(t)
	if len(chunks) != 1 {
		t.Fatalf("got %d EOF chunks, want 1", len(chunks))
	}
	if !chunks[0].EOF || chunks[0].Err != "boom" {
		t.Fatalf("EOF chunk = %+v, want the first Close's cause", chunks[0])
	}
}

// An event spelled like a reply would be matched by the host against a command
// that never existed, so Emit must refuse result kinds outright.
func TestEmitRefusesResultKinds(t *testing.T) {
	rec := &recordingSender{}
	err := NewEmitter(rec).Emit(context.Background(),
		weavewire.ResultKind(weavewire.KindPresenceHello), nil)
	if err == nil {
		t.Fatal("a result kind was emitted as an event")
	}
	if !strings.Contains(err.Error(), "result kind") {
		t.Fatalf("unhelpful error: %v", err)
	}
	if len(rec.sent) != 0 {
		t.Fatal("the refused event still reached the transport")
	}
}

func TestEmitAddressesTheHypervisorPeer(t *testing.T) {
	rec := &recordingSender{}
	if err := NewEmitter(rec).Emit(context.Background(), "weave.exec.exit",
		map[string]any{"code": 0}); err != nil {
		t.Fatal(err)
	}
	if len(rec.sent) != 1 {
		t.Fatalf("sent %d messages", len(rec.sent))
	}
	if rec.sent[0].Peer != modulesdk.PeerHypervisor {
		t.Fatalf("event peer = %v, want hypervisor", rec.sent[0].Peer)
	}
	if rec.sent[0].Kind != "weave.exec.exit" {
		t.Fatalf("event kind = %q", rec.sent[0].Kind)
	}
}
