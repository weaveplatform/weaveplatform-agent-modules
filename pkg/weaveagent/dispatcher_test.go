package weaveagent

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// captureSender records every reply the dispatcher sends.
type captureSender struct {
	mu   sync.Mutex
	sent []modulesdk.Message
}

func (c *captureSender) Send(_ context.Context, msg modulesdk.Message, _ bool) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return true, nil
}

func (c *captureSender) messages() []modulesdk.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]modulesdk.Message(nil), c.sent...)
}

// runOne dispatches a single command and returns the captured replies.
func runOne(t *testing.T, d *Dispatcher, cmd modulesdk.Message) []modulesdk.Message {
	t.Helper()
	cap, _ := d.send.(*captureSender)
	in := make(chan modulesdk.Message, 1)
	in <- cmd
	close(in)
	d.Run(context.Background(), in)
	return cap.messages()
}

func mustCommand(t *testing.T, kind, id string, payload any) modulesdk.Message {
	t.Helper()
	data, err := weavewire.EncodeCommand(id, payload)
	if err != nil {
		t.Fatal(err)
	}
	return modulesdk.Message{Peer: modulesdk.PeerHypervisor, Kind: kind, Data: data}
}

func TestDispatcherRoundTrip(t *testing.T) {
	cs := &captureSender{}
	d := New(cs, nil)
	d.Handle("weave.echo", func(_ context.Context, payload []byte) ([]byte, error) {
		return payload, nil // echo the request payload straight back
	})

	replies := runOne(
		t,
		d,
		mustCommand(t, "weave.echo", "abc", map[string]string{"hi": "there"}),
	)
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	r := replies[0]
	if r.Kind != "weave.echo.result" {
		t.Fatalf("reply kind = %q, want weave.echo.result", r.Kind)
	}
	if r.Peer != modulesdk.PeerHypervisor {
		t.Fatalf("reply peer = %v, want Hypervisor", r.Peer)
	}
	var res weavewire.Result
	if err := json.Unmarshal(r.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.ID != "abc" {
		t.Fatalf("correlation id = %q, want abc", res.ID)
	}
	if res.Err != "" {
		t.Fatalf("unexpected err: %s", res.Err)
	}
	var got map[string]string
	if err := json.Unmarshal(res.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got["hi"] != "there" {
		t.Fatalf("payload round-trip lost: %+v", got)
	}
}

func TestDispatcherUnknownKind(t *testing.T) {
	cs := &captureSender{}
	d := New(cs, nil)

	replies := runOne(t, d, mustCommand(t, "weave.nope", "id1", nil))
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	var res weavewire.Result
	if err := json.Unmarshal(replies[0].Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.ID != "id1" || res.Err == "" {
		t.Fatalf("unknown kind must reply with the id and a non-empty err: %+v", res)
	}
}

func TestDispatcherIgnoresResultEchoes(t *testing.T) {
	cs := &captureSender{}
	d := New(cs, nil)
	d.Handle("weave.echo", func(_ context.Context, p []byte) ([]byte, error) { return p, nil })

	// A message whose kind is itself a result must not be dispatched.
	replies := runOne(t, d, modulesdk.Message{
		Peer: modulesdk.PeerHypervisor, Kind: "weave.echo.result", Data: []byte(`{"id":"x"}`),
	})
	if len(replies) != 0 {
		t.Fatalf("result echoes must be ignored, got %d replies", len(replies))
	}
}

func TestHandleRejectsResultKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering a handler for a .result kind must panic")
		}
	}()
	New(
		&captureSender{},
		nil,
	).Handle("weave.echo.result", func(context.Context, []byte) ([]byte, error) { return nil, nil })
}

// A stream's chunks and the EOF that ends it must be applied in the order the
// host sent them.
//
// This is the bug that made `weave exec -i` deliver nothing to the guest process
// while reporting success: the dispatcher ran every message on its own
// goroutine, the EOF won, the pipe closed, and every chunk after it failed with
// "file already closed". The loopback suite had the same race and almost always
// won it, so it looked like a host-side problem for a while.
//
// The assertion is on ORDER, not on timing: a handler that sleeps on the first
// message would reorder under any concurrent dispatcher, and cannot under a
// serial one.
func TestStdinIsAppliedInTheOrderItWasSent(t *testing.T) {
	var mu sync.Mutex
	var seen []string

	d := New(&captureSender{}, nil)
	d.Handle(weavewire.KindExecStdin, func(_ context.Context, payload []byte) ([]byte, error) {
		var chunk weavewire.Chunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return nil, err
		}
		// The first message is slow. Under a goroutine-per-message dispatcher
		// the EOF overtakes it; under a serial one it cannot.
		if chunk.Seq == 0 {
			time.Sleep(150 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		if chunk.EOF {
			seen = append(seen, "eof")
		} else {
			seen = append(seen, string(chunk.Data))
		}
		return nil, nil
	})

	in := make(chan modulesdk.Message, 4)
	for _, c := range []weavewire.Chunk{
		{StreamID: "e1", Seq: 0, Data: []byte("first")},
		{StreamID: "e1", Seq: 1, Data: []byte("second")},
		{StreamID: "e1", Seq: 2, EOF: true},
	} {
		data, err := weavewire.EncodeCommand("", c)
		if err != nil {
			t.Fatal(err)
		}
		in <- modulesdk.Message{Peer: modulesdk.PeerHypervisor, Kind: weavewire.KindExecStdin, Data: data}
	}
	close(in)

	d.Run(context.Background(), in)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"first", "second", "eof"}
	if !slices.Equal(seen, want) {
		t.Fatalf("applied %v, want %v — the EOF must not overtake the data it ends", seen, want)
	}
}
