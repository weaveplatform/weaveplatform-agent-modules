package weaveclient_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/internal/cliptransfer"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// diskClipboard is a guest clipboard whose files are on disk: it offers them by
// path and takes staged ones by path, as the OS backends do.
type diskClipboard struct {
	mu    sync.Mutex
	token uint64
	items []weavewire.ClipboardItem
	files []weaveclipboard.File
}

func (d *diskClipboard) Stat(context.Context) (weavewire.ClipboardStatResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return weavewire.ClipboardStatResponse{ChangeToken: d.token}, nil
}

func (d *diskClipboard) Read(
	context.Context, []weavewire.ClipboardFormat, int64,
) (weaveclipboard.Contents, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return weaveclipboard.Contents{
		ChangeToken: d.token, Items: slices.Clone(d.items), Files: slices.Clone(d.files),
	}, nil
}

func (d *diskClipboard) Write(
	ctx context.Context, items []weavewire.ClipboardItem,
) (weavewire.ClipboardSetResponse, error) {
	return d.WriteFiles(ctx, items, nil)
}

func (d *diskClipboard) WriteFiles(
	_ context.Context, items []weavewire.ClipboardItem, paths []string,
) (weavewire.ClipboardSetResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.token++
	d.items, d.files = nil, nil
	res := weavewire.ClipboardSetResponse{ChangeToken: d.token}
	for _, it := range items {
		if it.Format != weavewire.ClipboardFiles {
			d.items = append(d.items, it)
		}
		if !slices.Contains(res.Written, it.Format) {
			res.Written = append(res.Written, it.Format)
		}
	}
	d.files = weaveclipboard.FilesAt(paths)
	return res, nil
}

func wireClipboard(t *testing.T) (*weaveclient.Client, *diskClipboard) {
	t.Helper()
	b := &diskClipboard{}
	return wire(t, weaveclipboard.NewService(b, weaveclipboard.WithStagingDir(t.TempDir()))), b
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func timeoutCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// withGuestFree makes every disk in the test report free bytes.
func withGuestFree(t *testing.T, free int64) {
	t.Helper()
	old := cliptransfer.FreeBytes
	cliptransfer.FreeBytes = func(string) (int64, error) { return free, nil }
	t.Cleanup(func() { cliptransfer.FreeBytes = old })
}

// Files cross both ways streamed from and to disk, each verified by its
// SHA-256, paced and reported as they go; the other representations cross
// with them, inline or streamed.
func TestClipboardStreamsFilesBothWays(t *testing.T) {
	client, b := wireClipboard(t)
	ctx := timeoutCtx(t)
	host := t.TempDir()
	file := bytes.Repeat([]byte("weave file "), (3<<20)/11)
	path := filepath.Join(host, "copied.bin")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	png := bytes.Repeat([]byte{0x89}, 2<<20)
	var paced, moved atomic.Int64
	opts := weaveclient.TransferOptions{
		Pace:     func(_ context.Context, n int) error { paced.Add(int64(n)); return nil },
		Progress: func(n int64) { moved.Add(n) },
	}

	st, err := client.ClipboardStat(ctx)
	if err != nil || !st.Streaming {
		t.Fatalf("stat %+v, %v", st, err)
	}
	res, err := client.ClipboardSend(ctx, []weaveclient.ClipboardSource{
		{Format: weavewire.ClipboardFiles, Name: "copied.bin", Path: path, Size: int64(len(file))},
		{Format: weavewire.ClipboardPNG, Data: png},
		{Format: weavewire.ClipboardText, Data: []byte("copied.bin")},
	}, opts)
	if err != nil || !res.Set || len(res.Dropped) != 0 || len(res.Written) != 3 {
		t.Fatalf("send %+v, %v", res, err)
	}
	total := int64(len(file) + len(png) + len("copied.bin"))
	if moved.Load() != total || paced.Load() != int64(len(file)+len(png)) {
		t.Errorf("moved %d, paced %d of %d", moved.Load(), paced.Load(), total)
	}
	if len(b.files) != 1 {
		t.Fatalf("the guest holds %+v", b.files)
	}
	if got, err := os.ReadFile(b.files[0].Path); err != nil || !bytes.Equal(got, file) {
		t.Fatalf("the guest staged %d bytes, %v", len(got), err)
	}

	// And back: files deferred and fetched, the image streamed with the reply.
	moved.Store(0)
	got, err := client.ClipboardGetWith(ctx, weavewire.ClipboardGetRequest{Stream: true}, opts)
	if err != nil || !got.Streamed || len(got.Items) != 3 {
		t.Fatalf("get %+v, %v", got, err)
	}
	if !got.Items[0].Deferred || got.Items[0].Size != int64(len(file)) ||
		!bytes.Equal(got.Items[1].Data, png) {
		t.Fatalf("items %+v", got.Items[0])
	}
	fetched, err := client.ClipboardFetch(ctx, "", 0, int64(len(file)), host, "back.bin", opts)
	if err == nil {
		t.Fatal("a fetch for no transfer")
	}
	transfer := lastTransfer(t, client, ctx, opts)
	fetched, err = client.ClipboardFetch(ctx, transfer, 0, int64(len(file)), host, "back.bin", opts)
	if err != nil || fetched.SHA256 != hexSum(file) ||
		fetched.Path != filepath.Join(host, "back.bin") {
		t.Fatalf("fetch %+v, %v", fetched, err)
	}
	if data, err := os.ReadFile(fetched.Path); err != nil || !bytes.Equal(data, file) {
		t.Fatalf("fetched %d bytes, %v", len(data), err)
	}
	if err := client.ClipboardCancel(transfer); err != nil {
		t.Fatal(err)
	}
}

// lastTransfer runs a streaming get under a transfer id of the test's own,
// so a fetch can name it.
func lastTransfer(
	t *testing.T,
	client *weaveclient.Client,
	ctx context.Context,
	opts weaveclient.TransferOptions,
) string {
	t.Helper()
	id := "test-transfer"
	if _, err := client.ClipboardGetWith(
		ctx,
		weavewire.ClipboardGetRequest{Stream: true, TransferID: id},
		opts,
	); err != nil {
		t.Fatal(err)
	}
	return id
}

// Each item is judged on its own: one the guest has no room for is dropped
// with its reason and the rest still cross; with nothing left, no set is made.
func TestClipboardSendDropsWhatDoesNotFit(t *testing.T) {
	client, b := wireClipboard(t)
	ctx := timeoutCtx(t)
	withGuestFree(t, cliptransfer.Reserve+(1<<20))
	big := make([]byte, 2<<20)
	res, err := client.ClipboardSend(ctx, []weaveclient.ClipboardSource{
		{Format: weavewire.ClipboardPNG, Data: big},
		{Format: weavewire.ClipboardText, Data: []byte("caption")},
		{
			Format: weavewire.ClipboardFiles,
			Name:   "gone",
			Path:   filepath.Join(t.TempDir(), "gone"),
			Size:   3,
		},
	}, weaveclient.TransferOptions{})
	if err != nil || !res.Set || len(res.Dropped) != 2 {
		t.Fatalf("send %+v, %v", res, err)
	}
	if d := res.Dropped[0]; d.Index != 0 || d.Err.Reason != weavewire.ClipboardReasonNoSpace {
		t.Errorf("dropped %+v", d)
	}
	if d := res.Dropped[1]; d.Index != 2 || d.Err.Reason != weavewire.ClipboardReasonUnreadable ||
		d.Err.Error() == "" {
		t.Errorf("dropped %+v", d)
	}
	if len(b.items) != 1 || string(b.items[0].Data) != "caption" {
		t.Errorf("the guest holds %+v", b.items)
	}
	// A companion naming the dropped item is rebuilt before the set.
	var seen []weaveclient.ClipboardDrop
	rebuilt, err := client.ClipboardSend(ctx, []weaveclient.ClipboardSource{
		{Format: weavewire.ClipboardFiles, Name: "big.bin", Data: big},
		{Format: weavewire.ClipboardText, Data: []byte("big.bin\nother")},
	}, weaveclient.TransferOptions{BeforeSet: func(
		items []weavewire.ClipboardItem, dropped []weaveclient.ClipboardDrop,
	) []weavewire.ClipboardItem {
		seen = dropped
		items[0].Data = []byte("other")
		return items
	}})
	if err != nil || !rebuilt.Set || len(seen) != 1 || len(b.items) != 1 ||
		string(b.items[0].Data) != "other" {
		t.Errorf("a rebuilt companion: %+v, %v; the guest holds %+v", rebuilt, err, b.items)
	}
	none, err := client.ClipboardSend(
		ctx,
		[]weaveclient.ClipboardSource{{Format: weavewire.ClipboardPNG, Data: big}},
		weaveclient.TransferOptions{BeforeSet: func(
			items []weavewire.ClipboardItem, _ []weaveclient.ClipboardDrop,
		) []weavewire.ClipboardItem {
			return items
		}},
	)
	if err != nil || none.Set || len(none.Dropped) != 1 {
		t.Errorf("a send of nothing that fits: %+v, %v", none, err)
	}

	// The host's disk is guarded the same way, before the guest is asked.
	_, err = client.ClipboardFetch(
		ctx,
		"t",
		0,
		2<<20,
		t.TempDir(),
		"f",
		weaveclient.TransferOptions{},
	)
	var te *weaveclient.TransferError
	if !errors.As(err, &te) || te.Reason != weavewire.ClipboardReasonNoSpace ||
		!errors.Is(err, weaveclient.ErrNoSpace) {
		t.Errorf("a fetch with no room: %v", err)
	}
	if _, err := client.ClipboardFetch(
		ctx,
		"t",
		0,
		1,
		t.TempDir(),
		"f",
		weaveclient.TransferOptions{Reserve: -1},
	); !errors.As(
		err,
		&te,
	) ||
		te.Reason != weavewire.ClipboardReasonUnreadable {
		t.Errorf("a fetch of nothing offered, with no reserve: %v", err)
	}
}

// Ending the context abandons a send, and the guest deletes what it staged.
func TestClipboardSendCancels(t *testing.T) {
	client, _ := wireClipboard(t)
	ctx, cancel := context.WithCancel(timeoutCtx(t))
	paced := make(chan struct{})
	_, err := client.ClipboardSend(
		ctx,
		[]weaveclient.ClipboardSource{{Format: weavewire.ClipboardPNG, Data: make([]byte, 1<<20)}},
		weaveclient.TransferOptions{Pace: func(ctx context.Context, _ int) error {
			select {
			case <-paced:
			default:
				close(paced)
				cancel()
			}
			<-ctx.Done()
			return ctx.Err()
		}},
	)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled send: %v", err)
	}
	if _, err := client.ClipboardSend(
		ctx,
		[]weaveclient.ClipboardSource{{Format: weavewire.ClipboardPNG, Data: make([]byte, 1<<20)}},
		weaveclient.TransferOptions{},
	); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Errorf("a send on a cancelled context: %v", err)
	}
}

// Against a hand-driven guest: what a guest that breaks the protocol, or
// stops, costs.
func TestClipboardFetchAgainstABrokenGuest(t *testing.T) {
	ctx := timeoutCtx(t)
	answer := func(g *rawGuest, size int64, then func(g *rawGuest, id string)) {
		go func() {
			env := g.read()
			var cmd weavewire.Command
			_ = json.Unmarshal(env.Data, &cmd)
			var req weavewire.ClipboardFetchRequest
			_ = json.Unmarshal(cmd.Payload, &req)
			p, _ := json.Marshal(weavewire.ClipboardFetchResponse{Size: size})
			g.reply(env, weavewire.Result{Payload: p})
			then(g, req.StreamID)
		}()
	}
	chunk := func(g *rawGuest, c weavewire.Chunk) {
		data, _ := json.Marshal(c)
		g.write(
			hvchannel.Envelope{
				Module: "weave.clipboard",
				Kind:   weavewire.KindClipboardDownload,
				Data:   data,
			},
		)
	}
	opts := weaveclient.TransferOptions{Idle: 100 * time.Millisecond, Reserve: -1}
	var te *weaveclient.TransferError

	g, client := newRaw(t)
	answer(g, 5, func(*rawGuest, string) {})
	if _, err := client.ClipboardFetch(
		ctx,
		"t",
		0,
		5,
		t.TempDir(),
		"f",
		opts,
	); !errors.Is(
		err,
		cliptransfer.ErrStalled,
	) {
		t.Errorf("a guest that stops: %v", err)
	}

	g, client = newRaw(t)
	answer(g, 5, func(g *rawGuest, id string) {
		chunk(g, weavewire.Chunk{StreamID: id, Data: []byte("hello")})
		chunk(g, weavewire.Chunk{StreamID: id, Seq: 1, EOF: true, Digest: hexSum([]byte("other"))})
	})
	if _, err := client.ClipboardFetch(
		ctx,
		"t",
		0,
		5,
		t.TempDir(),
		"f",
		opts,
	); !errors.As(
		err,
		&te,
	) ||
		te.Reason != weavewire.ClipboardReasonIntegrity {
		t.Errorf("a wrong digest: %v", err)
	}

	g, client = newRaw(t)
	answer(g, 6, func(g *rawGuest, _ string) {
		if env := g.read(); env.Kind != weavewire.KindClipboardCancel {
			t.Errorf("after a size mismatch the host sent %s", env.Kind)
		}
	})
	if _, err := client.ClipboardFetch(
		ctx,
		"t",
		0,
		5,
		t.TempDir(),
		"f",
		opts,
	); !errors.As(
		err,
		&te,
	) ||
		te.Reason != weavewire.ClipboardReasonUnreadable {
		t.Errorf("a size mismatch: %v", err)
	}

	// A guest that sends past its window is not trusted with the rest.
	g, client = newRaw(t)
	release := make(chan struct{})
	answer(g, 4<<20, func(g *rawGuest, id string) {
		for i := range 128 {
			chunk(
				g,
				weavewire.Chunk{
					StreamID: id,
					Seq:      uint64(i),
					Data:     make([]byte, weavewire.MaxChunkBytes),
				},
			)
		}
		close(release)
		drain(g)
	})
	held := weaveclient.TransferOptions{
		Reserve: -1,
		Pace:    func(context.Context, int) error { <-release; return nil },
	}
	if _, err := client.ClipboardFetch(
		ctx,
		"t",
		0,
		4<<20,
		t.TempDir(),
		"f",
		held,
	); !errors.As(
		err,
		&te,
	) ||
		te.Reason != weavewire.ClipboardReasonIntegrity {
		t.Errorf("an overrun: %v", err)
	}

	// The pace failing, the context ending and the channel closing each end
	// a fetch.
	for name, tc := range map[string]struct {
		opts   weaveclient.TransferOptions
		ctx    func() context.Context
		closes bool
	}{
		"pace": {opts: weaveclient.TransferOptions{Reserve: -1, Pace: func(context.Context, int) error { return errors.New("paced out") }}},
		"ctx": {opts: opts, ctx: func() context.Context {
			c, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			t.Cleanup(cancel)
			return c
		}},
		"closed": {opts: weaveclient.TransferOptions{Reserve: -1}, closes: true},
	} {
		g, client := newRaw(t)
		answer(g, 1<<20, func(g *rawGuest, id string) {
			if tc.closes {
				_ = g.conn.Close()
				return
			}
			for i := range 9 {
				chunk(
					g,
					weavewire.Chunk{
						StreamID: id,
						Seq:      uint64(i),
						Data:     make([]byte, weavewire.MaxChunkBytes),
					},
				)
			}
			drain(g)
		})
		c := ctx
		if tc.ctx != nil {
			c = tc.ctx()
		}
		if _, err := client.ClipboardFetch(
			c,
			"t",
			0,
			1<<20,
			t.TempDir(),
			"f",
			tc.opts,
		); err == nil {
			t.Errorf("%s: fetched", name)
		}
	}
}

func TestClipboardSendAgainstABrokenGuest(t *testing.T) {
	ctx := timeoutCtx(t)
	stageReply := func(g *rawGuest, st weavewire.ClipboardStageResponse) string {
		env := g.read()
		var cmd weavewire.Command
		_ = json.Unmarshal(env.Data, &cmd)
		var req weavewire.ClipboardStageRequest
		_ = json.Unmarshal(cmd.Payload, &req)
		p, _ := json.Marshal(st)
		g.reply(env, weavewire.Result{Payload: p})
		return req.StreamID
	}
	staged := func(g *rawGuest, ev weavewire.ClipboardStaged) {
		data, _ := json.Marshal(ev)
		g.write(
			hvchannel.Envelope{
				Module: "weave.clipboard",
				Kind:   weavewire.KindClipboardStaged,
				Data:   data,
			},
		)
	}
	item := []weaveclient.ClipboardSource{
		{Format: weavewire.ClipboardPNG, Data: make([]byte, 300<<10)},
	}
	opts := weaveclient.TransferOptions{Idle: 100 * time.Millisecond}

	// The guest drops it as it arrives: the item is dropped with its reason.
	g, client := newRaw(t)
	go func(g *rawGuest) {
		id := stageReply(g, weavewire.ClipboardStageResponse{})
		g.read() // the first chunk
		staged(g, weavewire.ClipboardStaged{StreamID: "someone else", Done: true})
		g.write(
			hvchannel.Envelope{
				Module: "weave.clipboard",
				Kind:   weavewire.KindClipboardStaged,
				Data:   []byte("{"),
			},
		)
		staged(
			g,
			weavewire.ClipboardStaged{
				StreamID: id,
				Acked:    10,
				Done:     true,
				Reason:   weavewire.ClipboardReasonNoSpace,
				Err:      "full",
			},
		)
		drain(g)
	}(g)
	res, err := client.ClipboardSend(ctx, item, opts)
	if err != nil || res.Set || len(res.Dropped) != 1 ||
		res.Dropped[0].Err.Reason != weavewire.ClipboardReasonNoSpace {
		t.Errorf("a guest that ran out of room: %+v, %v", res, err)
	}

	// A guest that stops acknowledging stalls the transfer, which is the
	// channel's failure: the whole send fails, and the guest is told.
	g, client = newRaw(t)
	cancelled := make(chan struct{})
	go func(g *rawGuest) {
		stageReply(g, weavewire.ClipboardStageResponse{})
		for {
			env, err := hvchannel.ReadEnvelope(g.r)
			if err != nil {
				return
			}
			if env.Kind == weavewire.KindClipboardCancel {
				close(cancelled)
				drain(g)
				return
			}
		}
	}(g)
	if _, err := client.ClipboardSend(ctx, item, opts); !errors.Is(err, cliptransfer.ErrStalled) {
		t.Errorf("a guest that stops: %v", err)
	}
	<-cancelled

	// A guest that refuses the stage call, or the set, fails the send.
	g, client = newRaw(t)
	go func(g *rawGuest) {
		g.reply(g.read(), weavewire.Result{Err: "no"})
		drain(g)
	}(g)
	if _, err := client.ClipboardSend(ctx, item, opts); err == nil {
		t.Error("a refused stage")
	}
	g, client = newRaw(t)
	go func(g *rawGuest) {
		g.reply(g.read(), weavewire.Result{Err: "no"})
		drain(g)
	}(g)
	if _, err := client.ClipboardSend(
		ctx,
		[]weaveclient.ClipboardSource{{Format: weavewire.ClipboardText, Data: []byte("x")}},
		opts,
	); err == nil {
		t.Error("a refused set")
	}
}

// A streaming get's credits are paced; a pace that fails stops crediting, and
// the stream stalls rather than outrunning the policy.
func TestStreamingGetCreditsArePaced(t *testing.T) {
	client, b := wireClipboard(t)
	b.items = []weavewire.ClipboardItem{{Format: weavewire.ClipboardPNG, Data: make([]byte, 3<<20)}}
	ctx := timeoutCtx(t)
	got, err := client.ClipboardGetWith(ctx, weavewire.ClipboardGetRequest{Stream: true},
		weaveclient.TransferOptions{Pace: func(context.Context, int) error { return nil }})
	if err != nil || len(got.Items) != 1 || len(got.Items[0].Data) != 3<<20 {
		t.Fatalf("get %d items, %v", len(got.Items), err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := client.ClipboardGetWith(
		short,
		weavewire.ClipboardGetRequest{Stream: true},
		weaveclient.TransferOptions{
			Pace: func(context.Context, int) error { return errors.New("paced out") },
		},
	); err == nil {
		t.Error("a get whose credits are refused finished")
	}
}

// A streaming get checks what arrived against the digest the guest sent.
func TestStreamingGetChecksTheDigest(t *testing.T) {
	g, client := newRaw(t)
	ctx := timeoutCtx(t)
	go func() {
		env := g.read()
		var cmd weavewire.Command
		_ = json.Unmarshal(env.Data, &cmd)
		var req weavewire.ClipboardGetRequest
		_ = json.Unmarshal(cmd.Payload, &req)
		p, _ := json.Marshal(
			weavewire.ClipboardGetResponse{Streamed: true, Items: []weavewire.ClipboardItem{
				{Format: weavewire.ClipboardPNG, Size: 5},
			}},
		)
		g.reply(env, weavewire.Result{Payload: p})
		data, _ := json.Marshal(
			weavewire.Chunk{
				StreamID: req.TransferID,
				Data:     []byte("hello"),
				EOF:      true,
				Digest:   "wrong",
			},
		)
		g.write(
			hvchannel.Envelope{
				Module: "weave.clipboard",
				Kind:   weavewire.KindClipboardDownload,
				Data:   data,
			},
		)
	}()
	if _, err := client.ClipboardGetWith(
		ctx,
		weavewire.ClipboardGetRequest{Stream: true},
		weaveclient.TransferOptions{},
	); !errors.Is(
		err,
		weaveclient.ErrTransfer,
	) {
		t.Errorf("a wrong digest: %v", err)
	}
}

// drain reads and discards what the host sends from here on — its credits —
// so a synchronous pipe never blocks it.
func drain(g *rawGuest) { _, _ = io.Copy(io.Discard, g.r) }
