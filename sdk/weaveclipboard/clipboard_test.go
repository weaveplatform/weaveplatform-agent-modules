package weaveclipboard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// memClipboard is a clipboard in memory: every write bumps the token, as the
// OS counters do.
type memClipboard struct {
	mu       sync.Mutex
	token    uint64
	items    []weavewire.ClipboardItem
	written  [][]weavewire.ClipboardItem
	err      error
	skipOver bool // leave representations over maxBytes unread, as a backend may
	asked    []weavewire.ClipboardFormat
}

func (m *memClipboard) Stat(context.Context) (weavewire.ClipboardStatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return weavewire.ClipboardStatResponse{}, m.err
	}
	st := weavewire.ClipboardStatResponse{ChangeToken: m.token}
	for _, it := range m.items {
		st.Formats = append(
			st.Formats,
			weavewire.ClipboardFormatInfo{Format: it.Format, Size: int64(len(it.Data))},
		)
	}
	return st, nil
}

func (m *memClipboard) Read(
	_ context.Context,
	formats []weavewire.ClipboardFormat,
	maxBytes int64,
) (weaveclipboard.Contents, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = formats
	if m.err != nil {
		return weaveclipboard.Contents{}, m.err
	}
	c := weaveclipboard.Contents{ChangeToken: m.token}
	for _, it := range m.items {
		if m.skipOver && int64(len(it.Data)) > maxBytes {
			it = weavewire.ClipboardItem{
				Format: it.Format,
				Name:   it.Name,
				Size:   int64(len(it.Data)),
			}
		}
		// Deliberately ignores formats: the service must still filter.
		c.Items = append(c.Items, it)
	}
	return c, nil
}

func (m *memClipboard) Write(
	_ context.Context,
	items []weavewire.ClipboardItem,
) (weavewire.ClipboardSetResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return weavewire.ClipboardSetResponse{}, m.err
	}
	m.token++
	m.items = items
	m.written = append(m.written, items)
	res := weavewire.ClipboardSetResponse{ChangeToken: m.token}
	for _, it := range items {
		res.Written = append(res.Written, it.Format)
	}
	return res, nil
}

func start(t *testing.T, b *memClipboard) *weavemoduletest.Harness {
	t.Helper()
	return weavemoduletest.Start(t, weaveclipboard.NewService(b))
}

func text(s string) weavewire.ClipboardItem {
	return weavewire.ClipboardItem{Format: weavewire.ClipboardText, Data: []byte(s)}
}

func TestServesExactlyTheClipboardContract(t *testing.T) {
	if err := weavemodule.CheckParity(weaveclipboard.NewService(&memClipboard{})); err != nil {
		t.Fatal(err)
	}
}

func TestStatReportsTheTokenAndFormats(t *testing.T) {
	b := &memClipboard{token: 7, items: []weavewire.ClipboardItem{text("hi")}}
	var st weavewire.ClipboardStatResponse
	start(t, b).Decode(weavewire.KindClipboardStat, nil, &st)
	if st.ChangeToken != 7 || len(st.Formats) != 1 || st.Formats[0].Size != 2 {
		t.Fatalf("stat = %+v", st)
	}
}

func TestBackendFailuresReachTheHost(t *testing.T) {
	b := &memClipboard{err: errors.New("no display server")}
	h := start(t, b)
	for kind, payload := range map[string]any{
		weavewire.KindClipboardStat: nil,
		weavewire.KindClipboardGet:  weavewire.ClipboardGetRequest{},
		weavewire.KindClipboardSet:  weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{text("x")}},
	} {
		if res := h.Call(kind, payload); !strings.Contains(res.Err, "no display server") {
			t.Errorf("%s: err = %q", kind, res.Err)
		}
	}
}

func TestGetInlineAppliesTheHostsFormats(t *testing.T) {
	b := &memClipboard{token: 3, items: []weavewire.ClipboardItem{
		text("hello"),
		{Format: weavewire.ClipboardHTML, Data: []byte("<b>hello</b>")},
	}}
	var got weavewire.ClipboardGetResponse
	start(t, b).Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{
		Formats: []weavewire.ClipboardFormat{weavewire.ClipboardText},
	}, &got)
	if got.ChangeToken != 3 || got.Streamed || len(got.Items) != 1 ||
		string(got.Items[0].Data) != "hello" || got.Items[0].Size != 5 {
		t.Fatalf("get = %+v", got)
	}
	if !slices.Equal(b.asked, []weavewire.ClipboardFormat{weavewire.ClipboardText}) {
		t.Fatalf("the backend was asked for %v", b.asked)
	}
}

// Over the cap is listed, never truncated: half an image is not an image.
func TestGetOmitsWhatIsOverTheCap(t *testing.T) {
	for _, skip := range []bool{false, true} {
		b := &memClipboard{skipOver: skip, items: []weavewire.ClipboardItem{
			text("short"),
			{Format: weavewire.ClipboardPNG, Data: bytes.Repeat([]byte{1}, 100)},
		}}
		var got weavewire.ClipboardGetResponse
		start(
			t,
			b,
		).Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{MaxBytes: 10}, &got)
		if len(got.Items) != 1 || got.Items[0].Format != weavewire.ClipboardText {
			t.Fatalf("skip=%v items = %+v", skip, got.Items)
		}
		if len(got.Omitted) != 1 || got.Omitted[0].Size != 100 || got.Omitted[0].Data != nil {
			t.Fatalf("skip=%v omitted = %+v", skip, got.Omitted)
		}
	}
}

// A whole get is bounded too, whatever the per-item cap says.
func TestGetBoundsTheWholeRead(t *testing.T) {
	half := bytes.Repeat([]byte{2}, weavewire.MaxClipboardBytes/2+1)
	b := &memClipboard{items: []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardPNG, Data: half},
		{Format: weavewire.ClipboardTIFF, Data: half},
	}}
	h := start(t, b)
	var got weavewire.ClipboardGetResponse
	h.Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{TransferID: "t"}, &got)
	if len(got.Items) != 1 || len(got.Omitted) != 1 ||
		got.Omitted[0].Format != weavewire.ClipboardTIFF {
		t.Fatalf("items = %d, omitted = %+v", len(got.Items), got.Omitted)
	}
}

func TestGetOverTheInlineLimitNeedsATransferID(t *testing.T) {
	big := bytes.Repeat([]byte("x"), weavewire.ClipboardInlineBytes+1)
	b := &memClipboard{
		items: []weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Data: big}},
	}
	res := start(t, b).Call(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{})
	if !strings.Contains(res.Err, "too large") {
		t.Fatalf("err = %q", res.Err)
	}
}

// Large content streams after the reply: the manifest first, then every
// item's bytes in order, ending with EOF.
func TestGetStreamsLargeContentAfterTheReply(t *testing.T) {
	png := bytes.Repeat([]byte{0x89}, weavewire.ClipboardInlineBytes)
	html := []byte("<img>")
	b := &memClipboard{items: []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardPNG, Data: png},
		{Format: weavewire.ClipboardHTML, Data: html},
	}}
	h := start(t, b)
	res := h.Call(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{TransferID: "tx-1"})
	if res.Err != "" {
		t.Fatal(res.Err)
	}
	var got weavewire.ClipboardGetResponse
	if err := json.Unmarshal(res.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Streamed || len(got.Items) != 2 || got.Items[0].Data != nil ||
		got.Items[0].Size != int64(len(png)) || got.Items[1].Size != int64(len(html)) {
		t.Fatalf("manifest = %+v", got)
	}

	body, replyFirst := collectDownload(t, h, "tx-1")
	if !bytes.Equal(body, append(slices.Clone(png), html...)) {
		t.Fatalf("streamed %d bytes, want %d", len(body), len(png)+len(html))
	}
	if !replyFirst {
		t.Fatal("a chunk went out before the reply")
	}
}

// collectDownload waits for a download stream's EOF and reassembles it,
// reporting whether the get's reply was sent before any chunk.
func collectDownload(t *testing.T, h *weavemoduletest.Harness, id string) ([]byte, bool) {
	t.Helper()
	isEOF := func(sent []modulesdk.Message) bool {
		for _, m := range sent {
			var c weavewire.Chunk
			if m.Kind == weavewire.KindClipboardDownload && json.Unmarshal(m.Data, &c) == nil &&
				c.EOF {
				return true
			}
		}
		return false
	}
	if !h.T.WaitFor(10*time.Second, isEOF) {
		t.Fatal("the download never ended")
	}
	var (
		asm        weavewire.StreamAssembler
		body       []byte
		sawReply   bool
		replyFirst = true
	)
	for _, m := range h.T.Sent() {
		switch m.Kind {
		case weavewire.ResultKind(weavewire.KindClipboardGet):
			sawReply = true
		case weavewire.KindClipboardDownload:
			if !sawReply {
				replyFirst = false
			}
			var c weavewire.Chunk
			if err := json.Unmarshal(m.Data, &c); err != nil || c.StreamID != id {
				t.Fatalf("chunk %+v, %v", c, err)
			}
			data, err := asm.Accept(c)
			if err != nil {
				t.Fatal(err)
			}
			body = append(body, data...)
		}
	}
	if !asm.Done() || asm.Err() != nil {
		t.Fatalf("done=%v err=%v", asm.Done(), asm.Err())
	}
	return body, replyFirst
}

// A stream that cannot be sent ends with an EOF carrying the failure, so the
// host never mistakes a short stream for the whole clipboard.
func TestGetStreamFailureEndsTheStreamWithTheCause(t *testing.T) {
	big := bytes.Repeat([]byte("y"), weavewire.ClipboardInlineBytes+1)
	b := &memClipboard{
		items: []weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Data: big}},
	}
	var failed bool
	h := weavemoduletest.Start(t, weaveclipboard.NewService(b), func(host *weavemoduletest.Host) {
		host.T.OnSend = func(msg modulesdk.Message) error {
			var c weavewire.Chunk
			if msg.Kind == weavewire.KindClipboardDownload && json.Unmarshal(msg.Data, &c) == nil &&
				!c.EOF && !failed {
				failed = true
				return errors.New("channel full")
			}
			return nil
		}
	})
	if res := h.Call(
		weavewire.KindClipboardGet,
		weavewire.ClipboardGetRequest{TransferID: "t"},
	); res.Err != "" {
		t.Fatal(res.Err)
	}
	ok := h.T.WaitFor(10*time.Second, func(sent []modulesdk.Message) bool {
		for _, m := range sent {
			var c weavewire.Chunk
			if m.Kind == weavewire.KindClipboardDownload && json.Unmarshal(m.Data, &c) == nil &&
				c.EOF {
				return strings.Contains(c.Err, "channel full")
			}
		}
		return false
	})
	if !ok {
		t.Fatal("the stream did not end with the cause")
	}
}

func TestSetInline(t *testing.T) {
	b := &memClipboard{token: 1}
	var got weavewire.ClipboardSetResponse
	start(
		t,
		b,
	).Decode(weavewire.KindClipboardSet, weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{
		text("hi"), {Format: weavewire.ClipboardHTML, Data: []byte("<i>hi</i>"), Size: 9},
	}}, &got)
	if got.ChangeToken != 2 || len(got.Written) != 2 {
		t.Fatalf("set = %+v", got)
	}
	if len(b.items) != 2 || string(b.items[0].Data) != "hi" || b.items[0].Size != 2 {
		t.Fatalf("clipboard = %+v", b.items)
	}
}

func TestSetRefusesInconsistentRequests(t *testing.T) {
	h := start(t, &memClipboard{})
	big := bytes.Repeat([]byte("z"), weavewire.ClipboardInlineBytes+1)
	for name, tc := range map[string]struct {
		payload any
		want    string
	}{
		"garbage":        {"nope", "decoding"},
		"no items":       {weavewire.ClipboardSetRequest{}, "at least one"},
		"no format":      {weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{{Data: []byte("x")}}}, "no format"},
		"negative size":  {weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{{Format: "text/plain", Size: -1}}}, "negative"},
		"size mismatch":  {weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{{Format: "text/plain", Size: 3, Data: []byte("x")}}}, "declares"},
		"inline too big": {weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{{Format: "text/plain", Data: big}}}, "upload it"},
		"over the limit": {weavewire.ClipboardSetRequest{TransferID: "t", Items: []weavewire.ClipboardItem{
			{Format: "image/png", Size: weavewire.MaxClipboardBytes + 1},
		}}, "too large"},
		"no upload": {weavewire.ClipboardSetRequest{TransferID: "missing", Items: []weavewire.ClipboardItem{
			{Format: "image/png", Size: 1},
		}}, "nothing was uploaded"},
	} {
		if res := h.Call(
			weavewire.KindClipboardSet,
			tc.payload,
		); !strings.Contains(
			res.Err,
			tc.want,
		) {
			t.Errorf("%s: err = %q, want %q", name, res.Err, tc.want)
		}
	}
	if res := h.Call(weavewire.KindClipboardGet, "nope"); !strings.Contains(res.Err, "decoding") {
		t.Errorf("get garbage: err = %q", res.Err)
	}
}

func uploadChunks(h *weavemoduletest.Harness, id string, data []byte, eof bool) uint64 {
	var seq uint64
	for len(data) > 0 {
		n := min(len(data), weavewire.MaxChunkBytes)
		h.Notify(
			weavewire.KindClipboardUpload,
			weavewire.Chunk{StreamID: id, Seq: seq, Data: data[:n]},
		)
		seq++
		data = data[n:]
	}
	if eof {
		h.Notify(weavewire.KindClipboardUpload, weavewire.Chunk{StreamID: id, Seq: seq, EOF: true})
		seq++
	}
	return seq
}

// A large set uploads its bytes first and then names the upload; the set is
// ordered behind the chunks, so it always finds them all.
func TestSetFromAnUpload(t *testing.T) {
	b := &memClipboard{}
	h := start(t, b)
	png := bytes.Repeat([]byte{7}, 3*weavewire.MaxChunkBytes+5)
	txt := []byte("caption")
	uploadChunks(h, "up-1", append(slices.Clone(png), txt...), true)

	var got weavewire.ClipboardSetResponse
	h.Decode(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{TransferID: "up-1", Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardPNG, Size: int64(len(png))},
			{Format: weavewire.ClipboardText, Size: int64(len(txt))},
		}},
		&got,
	)
	if len(b.items) != 2 || !bytes.Equal(b.items[0].Data, png) ||
		string(b.items[1].Data) != "caption" {
		t.Fatalf("clipboard got %d items", len(b.items))
	}
	// The upload is consumed: naming it again fails.
	res := h.Call(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{TransferID: "up-1", Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardText, Size: 1},
		}},
	)
	if !strings.Contains(res.Err, "nothing was uploaded") {
		t.Fatalf("err = %q", res.Err)
	}
}

func TestSetFromABrokenUploadFails(t *testing.T) {
	item := []weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Size: 4}}
	for name, tc := range map[string]struct {
		send func(h *weavemoduletest.Harness)
		want string
	}{
		"unfinished": {func(h *weavemoduletest.Harness) { uploadChunks(h, "u", []byte("data"), false) }, "not finished"},
		"gap": {func(h *weavemoduletest.Harness) {
			h.Notify(weavewire.KindClipboardUpload, weavewire.Chunk{StreamID: "u", Seq: 1, Data: []byte("data")})
			h.Notify(weavewire.KindClipboardUpload, weavewire.Chunk{StreamID: "u", Seq: 2, EOF: true})
		}, "lost chunks"},
		"producer failed": {func(h *weavemoduletest.Harness) {
			h.Notify(weavewire.KindClipboardUpload, weavewire.Chunk{StreamID: "u", Data: []byte("da")})
			h.Notify(weavewire.KindClipboardUpload, weavewire.Chunk{StreamID: "u", Seq: 1, EOF: true, Err: "host read failed"})
		}, "host read failed"},
		"short": {func(h *weavemoduletest.Harness) { uploadChunks(h, "u", []byte("da"), true) }, "declare 4"},
		"oversized chunk": {func(h *weavemoduletest.Harness) {
			h.Notify(weavewire.KindClipboardUpload, weavewire.Chunk{
				StreamID: "u", Data: make([]byte, weavewire.MaxChunkBytes+1),
			})
		}, "size cap"},
	} {
		b := &memClipboard{}
		h := start(t, b)
		tc.send(h)
		res := h.Call(
			weavewire.KindClipboardSet,
			weavewire.ClipboardSetRequest{TransferID: "u", Items: item},
		)
		if !strings.Contains(res.Err, tc.want) {
			t.Errorf("%s: err = %q, want %q", name, res.Err, tc.want)
		}
		if len(b.written) != 0 {
			t.Errorf("%s: a broken upload was written", name)
		}
	}
}

// An upload past the whole-clipboard limit is refused as it arrives, without
// holding the bytes until a set claims it.
func TestUploadOverTheLimitIsRefused(t *testing.T) {
	h := start(t, &memClipboard{})
	chunk := make([]byte, weavewire.MaxChunkBytes)
	n := weavewire.MaxClipboardBytes/weavewire.MaxChunkBytes + 1
	for i := range n {
		h.Notify(
			weavewire.KindClipboardUpload,
			weavewire.Chunk{StreamID: "huge", Seq: uint64(i), Data: chunk},
		)
	}
	// A chunk after the failure is dropped too.
	h.Notify(
		weavewire.KindClipboardUpload,
		weavewire.Chunk{StreamID: "huge", Seq: uint64(n), EOF: true},
	)
	res := h.Call(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{TransferID: "huge", Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardPNG, Size: 1},
		}},
	)
	if !strings.Contains(res.Err, "too large") {
		t.Fatalf("err = %q", res.Err)
	}
}

// Uploads a host abandoned do not pile up: past the bound, the oldest goes.
func TestAbandonedUploadsAreBounded(t *testing.T) {
	h := start(t, &memClipboard{})
	for _, id := range []string{"a", "b", "c"} {
		uploadChunks(h, id, []byte("abcd"), true)
	}
	item := []weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Size: 4}}
	if res := h.Call(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{TransferID: "a", Items: item},
	); !strings.Contains(
		res.Err,
		"nothing was uploaded",
	) {
		t.Fatalf("the oldest upload was kept: %q", res.Err)
	}
	if res := h.Call(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{TransferID: "c", Items: item},
	); res.Err != "" {
		t.Fatalf("the newest upload was dropped: %q", res.Err)
	}
}

func TestMalformedUploadChunksAreDropped(t *testing.T) {
	h := start(t, &memClipboard{})
	h.Notify(weavewire.KindClipboardUpload, "garbage")
	h.Notify(weavewire.KindClipboardUpload, weavewire.Chunk{Data: []byte("no id")})
	// The module is still healthy and answering.
	if res := h.Call(weavewire.KindClipboardStat, nil); res.Err != "" {
		t.Fatal(res.Err)
	}
}
