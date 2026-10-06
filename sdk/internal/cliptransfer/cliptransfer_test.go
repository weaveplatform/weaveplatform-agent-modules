package cliptransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

var errInjected = errors.New("injected")

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// withFree makes the disk report free bytes for the test.
func withFree(t *testing.T, free func() (int64, error)) {
	t.Helper()
	old := FreeBytes
	FreeBytes = func(string) (int64, error) { return free() }
	t.Cleanup(func() { FreeBytes = old })
}

func TestFreeBytesOfARealDisk(t *testing.T) {
	n, err := freeBytes(t.TempDir())
	if err != nil || n <= 0 {
		t.Fatalf("free = %d, %v", n, err)
	}
	if _, err := freeBytes(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("the free space of a missing directory")
	}
}

func TestCheckSpace(t *testing.T) {
	withFree(t, func() (int64, error) { return 1000, nil })
	if free, err := CheckSpace("x", 900, 100); err != nil || free != 1000 {
		t.Errorf("room for it: %d, %v", free, err)
	}
	if free, err := CheckSpace("x", 901, 100); !errors.Is(err, ErrNoSpace) || free != 1000 {
		t.Errorf("one byte short: %d, %v", free, err)
	}
	withFree(t, func() (int64, error) { return 0, errInjected })
	if free, err := CheckSpace("x", 1<<40, 0); err != nil || free != -1 {
		t.Errorf("unknown free space is not a refusal: %d, %v", free, err)
	}
}

func TestFailure(t *testing.T) {
	f := &Failure{Reason: weavewire.ClipboardReasonIntegrity, Err: ErrIntegrity}
	if f.Error() != "integrity: not the item that was sent" ||
		ReasonOf(f, "x") != weavewire.ClipboardReasonIntegrity {
		t.Errorf("%q", f.Error())
	}
	wrapped := errors.Join(errInjected, &Failure{Reason: "r", Err: errInjected})
	if ReasonOf(wrapped, "x") != "r" || !errors.Is(&Failure{Err: errInjected}, errInjected) {
		t.Error("a wrapped failure's reason")
	}
	if ReasonOf(errInjected, "fallback") != "fallback" || (&Failure{Reason: "r"}).Error() != "r" {
		t.Error("fallback")
	}
}

func TestWindowWaitsForRoomAndEnds(t *testing.T) {
	ctx := context.Background()
	w := NewWindow()
	if err := w.Wait(ctx, weavewire.ClipboardWindowBytes-1, 0); err != nil {
		t.Fatalf("under the window: %v", err)
	}
	if err := w.Wait(
		ctx,
		weavewire.ClipboardWindowBytes,
		10*time.Millisecond,
	); !errors.Is(
		err,
		ErrStalled,
	) {
		t.Fatalf("a full window with nothing acknowledged: %v", err)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		w.Ack(10)
		w.Ack(5) // late, ignored
	}()
	if err := w.Wait(
		ctx,
		weavewire.ClipboardWindowBytes,
		time.Second,
	); err != nil ||
		w.Acked() != 10 {
		t.Fatalf("an acknowledgement opens it: %v, %d", err, w.Acked())
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := w.Wait(cctx, 1<<40, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait: %v", err)
	}
	w.End(nil)
	w.End(errInjected) // the first end is kept
	w.Ack(1 << 30)     // after the end, ignored
	if err := w.Wait(ctx, 0, 0); !errors.Is(err, ErrEnded) || w.Acked() != 10 {
		t.Errorf("a wait on an ended stream: %v", err)
	}
	if err := w.WaitEnd(ctx, 0); err != nil {
		t.Errorf("the verdict: %v", err)
	}

	failed := NewWindow()
	go failed.End(errInjected)
	if err := failed.WaitEnd(ctx, time.Second); !errors.Is(err, errInjected) {
		t.Errorf("a failed end: %v", err)
	}
	if err := failed.Wait(ctx, 0, 0); !errors.Is(err, errInjected) {
		t.Errorf("a wait on a failed stream: %v", err)
	}
	if err := NewWindow().WaitEnd(ctx, 5*time.Millisecond); !errors.Is(err, ErrStalled) {
		t.Errorf("an end that never comes: %v", err)
	}
}

// collect is an Emit that keeps what it is sent.
type collect struct {
	mu     sync.Mutex
	chunks []weavewire.Chunk
	fail   error
}

func (c *collect) emit(_ context.Context, ch weavewire.Chunk) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil && !ch.EOF {
		return c.fail
	}
	ch.Data = bytes.Clone(ch.Data)
	c.chunks = append(c.chunks, ch)
	return nil
}

func (c *collect) last() weavewire.Chunk {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chunks[len(c.chunks)-1]
}

func TestSenderStreamsExactlyItsSizeWithItsDigest(t *testing.T) {
	data := bytes.Repeat([]byte("weave"), weavewire.MaxChunkBytes) // five chunks
	w := NewWindow()
	var c collect
	var paced, progress int64
	got, err := Sender{
		Size: int64(len(data)), Window: w, Emit: c.emit,
		Pace:     func(_ context.Context, n int) error { paced += int64(n); return nil },
		Progress: func(n int64) { progress = n },
	}.Stream(context.Background(), bytes.NewReader(data))
	if err != nil || got != digest(data) {
		t.Fatalf("stream: %q, %v", got, err)
	}
	if paced != int64(len(data)) || progress != int64(len(data)) {
		t.Errorf("paced %d, progress %d", paced, progress)
	}
	var asm weavewire.StreamAssembler
	var body []byte
	for _, ch := range c.chunks {
		b, err := asm.Accept(ch)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, b...)
	}
	if !asm.Done() || !bytes.Equal(body, data) || c.last().Digest != digest(data) {
		t.Errorf("the stream carried %d bytes, done %v", len(body), asm.Done())
	}
}

func TestSenderEndsTheStreamOnEveryFailure(t *testing.T) {
	ctx := context.Background()
	data := []byte("0123456789")
	cases := map[string]struct {
		r      io.Reader
		size   int64
		pace   func(context.Context, int) error
		window func(*Window)
		want   string
	}{
		"short source": {
			r:    bytes.NewReader(data),
			size: 11,
			want: weavewire.ClipboardReasonUnreadable,
		},
		"long source": {
			r:    bytes.NewReader(data),
			size: 9,
			want: weavewire.ClipboardReasonUnreadable,
		},
		"read error": {
			r:    io.MultiReader(strings.NewReader("x"), errReader{}),
			size: 5,
			want: weavewire.ClipboardReasonUnreadable,
		},
		"pace error": {
			r: bytes.NewReader(data), size: 10,
			pace: func(context.Context, int) error { return errInjected },
		},
		"receiver ended": {
			r: bytes.NewReader(data), size: 10,
			window: func(w *Window) { w.End(&Failure{Reason: weavewire.ClipboardReasonNoSpace}) },
			want:   weavewire.ClipboardReasonNoSpace,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := NewWindow()
			if tc.window != nil {
				tc.window(w)
			}
			var c collect
			_, err := Sender{
				Size:   tc.size,
				Window: w,
				Emit:   c.emit,
				Pace:   tc.pace,
			}.Stream(
				ctx,
				tc.r,
			)
			if err == nil {
				t.Fatal("no failure")
			}
			if tc.want != "" && ReasonOf(err, "") != tc.want {
				t.Errorf("reason %q, want %q (%v)", ReasonOf(err, ""), tc.want, err)
			}
			if end := c.last(); !end.EOF || end.Err == "" {
				t.Errorf("the stream ended with %+v", end)
			}
		})
	}

	// A chunk that cannot be sent is the channel's failure, not the item's.
	c := collect{fail: errInjected}
	if _, err := (Sender{Size: 10, Window: NewWindow(), Emit: c.emit}).Stream(
		ctx,
		bytes.NewReader(data),
	); !errors.Is(
		err,
		errInjected,
	) {
		t.Errorf("a failing emit: %v", err)
	}
	c = collect{}
	if _, err := (Sender{Size: 0, Window: NewWindow(), Emit: func(context.Context, weavewire.Chunk) error {
		return errInjected
	}}).Stream(
		ctx,
		bytes.NewReader(nil),
	); !errors.Is(
		err,
		errInjected,
	) {
		t.Errorf("a failing EOF: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errInjected }

// chunks splits data into a stream's chunks, its EOF carrying digest.
func chunks(data []byte, digest string) []weavewire.Chunk {
	var out []weavewire.Chunk
	for len(data) > 0 {
		n := min(len(data), weavewire.MaxChunkBytes)
		out = append(out, weavewire.Chunk{Seq: uint64(len(out)), Data: data[:n]})
		data = data[n:]
	}
	return append(out, weavewire.Chunk{Seq: uint64(len(out)), EOF: true, Digest: digest})
}

func TestSinkPublishesOnlyAWholeVerifiedItem(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("x"), 3*weavewire.MaxChunkBytes+7)
	s, err := NewSink(dir, "f.bin", int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	cs := chunks(data, digest(data))
	for i, c := range cs {
		whole, err := s.Accept(c)
		if err != nil || whole != (i == len(cs)-1) {
			t.Fatalf("chunk %d: %v, %v", i, whole, err)
		}
		if !whole {
			if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
				t.Fatalf("the final name exists before the end: %v", err)
			}
		}
	}
	if got, err := os.ReadFile(s.Path()); err != nil || !bytes.Equal(got, data) ||
		s.Digest() != digest(data) || s.Written() != int64(len(data)) {
		t.Fatalf("published %d bytes, %v", len(got), err)
	}
	s.Abort() // after the end: nothing to remove
	if _, err := os.Stat(s.Path()); err != nil {
		t.Error("an abort after the end removed the item")
	}
	if _, err := s.Accept(
		weavewire.Chunk{Seq: 99},
	); ReasonOf(
		err,
		"",
	) != weavewire.ClipboardReasonIntegrity {
		t.Errorf("a chunk after the end: %v", err)
	}
}

func TestSinkDropsWhatIsNotTheItem(t *testing.T) {
	data := []byte("hello")
	cases := map[string]struct {
		chunks []weavewire.Chunk
		size   int64
		want   string
	}{
		"gap": {
			[]weavewire.Chunk{{Seq: 1, Data: data}},
			5,
			weavewire.ClipboardReasonIntegrity,
		},
		"over size": {
			[]weavewire.Chunk{{Seq: 0, Data: data}},
			4,
			weavewire.ClipboardReasonIntegrity,
		},
		"short": {chunks(data, digest(data)), 6, weavewire.ClipboardReasonIntegrity},
		"wrong digest": {
			chunks(data, digest([]byte("other"))),
			5,
			weavewire.ClipboardReasonIntegrity,
		},
		"no digest": {chunks(data, ""), 5, weavewire.ClipboardReasonIntegrity},
		"sender failed": {
			[]weavewire.Chunk{{Seq: 0, Data: data[:2]}, {Seq: 1, EOF: true, Err: "gone"}},
			5, weavewire.ClipboardReasonUnreadable,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := NewSink(dir, "f", tc.size, 0)
			if err != nil {
				t.Fatal(err)
			}
			var last error
			for _, c := range tc.chunks {
				if _, last = s.Accept(c); last != nil {
					break
				}
			}
			if ReasonOf(last, "") != tc.want {
				t.Errorf("reason %q, want %q (%v)", ReasonOf(last, ""), tc.want, last)
			}
			if left, _ := os.ReadDir(dir); len(left) != 0 {
				t.Errorf("left %v behind", left)
			}
		})
	}
}

func TestSinkGuardsTheDisk(t *testing.T) {
	dir := t.TempDir()
	free := int64(100)
	withFree(t, func() (int64, error) { return free, nil })
	if _, err := NewSink(dir, "f", 60, 50); ReasonOf(err, "") != weavewire.ClipboardReasonNoSpace {
		t.Fatalf("no room at the start: %v", err)
	}

	// Room at the start, and something else fills the disk mid-transfer.
	free = 1 << 40
	size := int64(SpaceCheckBytes + 10)
	s, err := NewSink(dir, "f", size, 50)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, weavewire.MaxChunkBytes)
	var seq uint64
	var last error
	for written := int64(0); written < SpaceCheckBytes && last == nil; written += int64(len(chunk)) {
		if written == SpaceCheckBytes-int64(len(chunk)) {
			free = 0
		}
		_, last = s.Accept(weavewire.Chunk{Seq: seq, Data: chunk})
		seq++
	}
	if ReasonOf(last, "") != weavewire.ClipboardReasonNoSpace {
		t.Fatalf("a disk that filled: %v", last)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left %v behind", left)
	}
}

func TestSinkFilesystemFailures(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewSink(
		filepath.Join(dir, "absent"),
		"f",
		1,
		0,
	); err == nil ||
		ReasonOf(err, "") != "" {
		t.Errorf("a sink in a missing directory: %v", err)
	}
	s, err := NewSink(dir, "f", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSink(dir, "f", 1, 0); err == nil {
		t.Error("two sinks on one partial file")
	}
	_ = s.f.Close() // the next write fails
	if _, err := s.Accept(
		weavewire.Chunk{Data: []byte("x")},
	); ReasonOf(
		err,
		"",
	) != weavewire.ClipboardReasonNoSpace {
		t.Errorf("a failing write: %v", err)
	}

	// The final name is taken by a directory: the rename fails.
	s, err = NewSink(dir, "g", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "g", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = s.Accept(weavewire.Chunk{EOF: true, Data: []byte("1"), Digest: digest([]byte("1"))})
	if err == nil {
		t.Fatal("published over a directory")
	}
	if _, serr := os.Stat(filepath.Join(dir, "g.part")); !os.IsNotExist(serr) {
		t.Errorf("the partial file was left: %v", serr)
	}
}
