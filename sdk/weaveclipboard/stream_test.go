package weaveclipboard_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/internal/cliptransfer"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// fileClipboard is memClipboard with files on disk: it offers its files by
// path, as the OS backends do, and takes staged files by path.
type fileClipboard struct {
	memClipboard
	fmu      sync.Mutex
	files    []weaveclipboard.File
	paths    [][]string
	writeErr error
}

func (f *fileClipboard) Read(
	ctx context.Context,
	formats []weavewire.ClipboardFormat,
	maxBytes int64,
) (weaveclipboard.Contents, error) {
	c, err := f.memClipboard.Read(ctx, formats, maxBytes)
	f.fmu.Lock()
	c.Files = slices.Clone(f.files)
	f.fmu.Unlock()
	return c, err
}

func (f *fileClipboard) WriteFiles(
	ctx context.Context,
	items []weavewire.ClipboardItem,
	paths []string,
) (weavewire.ClipboardSetResponse, error) {
	f.fmu.Lock()
	err := f.writeErr
	f.paths = append(f.paths, paths)
	f.fmu.Unlock()
	if err != nil {
		return weavewire.ClipboardSetResponse{}, err
	}
	return f.memClipboard.Write(ctx, items)
}

func (f *fileClipboard) lastPaths() []string {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	return f.paths[len(f.paths)-1]
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// writeFile writes data to dir/name and offers it as a copied file.
func writeFile(t *testing.T, dir, name string, data []byte) weaveclipboard.File {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return weaveclipboard.File{Name: name, Path: p, Size: int64(len(data))}
}

func startFiles(t *testing.T, b *fileClipboard) (*weavemoduletest.Harness, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "staging")
	return weavemoduletest.Start(
		t,
		weaveclipboard.NewService(b, weaveclipboard.WithStagingDir(root)),
	), root
}

// stream collects the chunks the guest sent under id, credits the host as it
// receives them, and returns their bytes and the EOF.
func stream(t *testing.T, h *weavemoduletest.Harness, id string) ([]byte, weavewire.Chunk) {
	t.Helper()
	var (
		asm    weavewire.StreamAssembler
		body   []byte
		end    weavewire.Chunk
		seen   int
		credit int64
	)
	for !asm.Done() {
		var got []weavewire.Chunk
		if !h.T.WaitFor(h.Timeout, func(sent []modulesdk.Message) bool {
			got = chunksOf(sent, weavewire.KindClipboardDownload, id)
			return len(got) > seen
		}) {
			t.Fatalf("stream %s stopped after %d chunks", id, seen)
		}
		for _, c := range got[seen:] {
			data, err := asm.Accept(c)
			if err != nil {
				t.Fatalf("stream %s: %v", id, err)
			}
			body = append(body, data...)
			end = c
		}
		seen = len(got)
		if int64(len(body))-credit >= weavewire.ClipboardCreditBytes {
			credit = int64(len(body))
			h.Notify(
				weavewire.KindClipboardCredit,
				weavewire.ClipboardCredit{StreamID: id, Acked: credit},
			)
		}
	}
	return body, end
}

func chunksOf(sent []modulesdk.Message, kind, id string) []weavewire.Chunk {
	var out []weavewire.Chunk
	for _, m := range sent {
		var c weavewire.Chunk
		if m.Kind == kind && json.Unmarshal(m.Data, &c) == nil && c.StreamID == id {
			out = append(out, c)
		}
	}
	return out
}

func stagedOf(sent []modulesdk.Message, id string) []weavewire.ClipboardStaged {
	var out []weavewire.ClipboardStaged
	for _, m := range sent {
		var ev weavewire.ClipboardStaged
		if m.Kind == weavewire.KindClipboardStaged && json.Unmarshal(m.Data, &ev) == nil &&
			ev.StreamID == id {
			out = append(out, ev)
		}
	}
	return out
}

func TestStatReportsStreaming(t *testing.T) {
	var st weavewire.ClipboardStatResponse
	start(t, &memClipboard{}).Decode(weavewire.KindClipboardStat, nil, &st)
	if !st.Streaming {
		t.Error("a service that streams does not say so")
	}
}

func TestFilesAtOffersRegularFilesOnly(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "a.txt", []byte("abc"))
	got := weaveclipboard.FilesAt([]string{f.Path, dir, filepath.Join(dir, "gone")})
	if len(got) != 1 || got[0] != f {
		t.Errorf("files %+v, want %+v", got, f)
	}
}

// A streaming get lists files as Deferred, sized and unread; each streams from
// disk when fetched, flow-controlled by the host's credits, with its digest.
func TestStreamingGetDefersFilesAndFetchStreamsThem(t *testing.T) {
	dir := t.TempDir()
	small := []byte("small file")
	big := bytes.Repeat([]byte("0123456789abcdef"), (3<<20)/16) // over the window
	b := &fileClipboard{
		memClipboard: memClipboard{token: 3, items: []weavewire.ClipboardItem{text("caption")}},
		files: []weaveclipboard.File{
			writeFile(t, dir, "small.txt", small),
			writeFile(t, dir, "over.iso", make([]byte, 600)),
			writeFile(t, dir, "big.bin", big),
		},
	}
	h, _ := startFiles(t, b)
	var got weavewire.ClipboardGetResponse
	h.Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{
		Stream: true, TransferID: "t1", MaxBytes: 4 << 20,
	}, &got)
	got = weavewire.ClipboardGetResponse{}
	h.Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{
		Stream: true, TransferID: "t1", MaxBytes: 500,
	}, &got)
	if len(got.Omitted) != 2 || got.Omitted[0].Name != "over.iso" ||
		got.Omitted[1].Name != "big.bin" {
		t.Fatalf("omitted %+v, want over.iso and big.bin over the cap", got.Omitted)
	}
	got = weavewire.ClipboardGetResponse{}
	h.Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{
		Stream: true, TransferID: "t2",
	}, &got)
	if got.Streamed || len(got.Items) != 4 || len(got.Omitted) != 0 {
		t.Fatalf("get %+v", got)
	}
	for i, want := range []string{"small.txt", "over.iso", "big.bin"} {
		if it := got.Items[i]; !it.Deferred || it.Name != want || it.Data != nil {
			t.Errorf("item %d = %+v, want %s deferred", i, it, want)
		}
	}
	if it := got.Items[3]; it.Deferred || string(it.Data) != "caption" {
		t.Errorf("the text came back as %+v", it)
	}

	for i, want := range map[int][]byte{0: small, 2: big} {
		id := "f" + string(rune('0'+i))
		var fr weavewire.ClipboardFetchResponse
		h.Decode(weavewire.KindClipboardFetch, weavewire.ClipboardFetchRequest{
			TransferID: "t2", Index: i, StreamID: id,
		}, &fr)
		if fr.Size != int64(len(want)) {
			t.Fatalf("fetch %d sized %d", i, fr.Size)
		}
		body, end := stream(t, h, id)
		if !bytes.Equal(body, want) || end.Digest != sum(want) || end.Err != "" {
			t.Errorf(
				"fetched %d bytes of %d, digest %q, err %q",
				len(body),
				len(want),
				end.Digest,
				end.Err,
			)
		}
	}
	// The window held the big file back until it was credited: the guest
	// never ran more than a window ahead of the host.
	if n := len(
		chunksOf(h.T.Sent(), weavewire.KindClipboardDownload, "f2"),
	); n < len(
		big,
	)/weavewire.MaxChunkBytes {
		t.Errorf("%d chunks", n)
	}
}

func TestFetchRefusesWhatWasNotOffered(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "a.txt", []byte("abc"))
	b := &fileClipboard{files: []weaveclipboard.File{f, writeFile(t, dir, "b.txt", []byte("b"))}}
	h, _ := startFiles(t, b)
	fetch := func(req weavewire.ClipboardFetchRequest) string { return h.Call(weavewire.KindClipboardFetch, req).Err }

	if err := fetch(weavewire.ClipboardFetchRequest{TransferID: "t", StreamID: "s"}); err == "" {
		t.Error("a fetch with no get")
	}
	var got weavewire.ClipboardGetResponse
	h.Decode(
		weavewire.KindClipboardGet,
		weavewire.ClipboardGetRequest{Stream: true, TransferID: "t"},
		&got,
	)
	for name, req := range map[string]weavewire.ClipboardFetchRequest{
		"no stream":      {TransferID: "t"},
		"another get":    {TransferID: "u", StreamID: "s"},
		"index too high": {TransferID: "t", Index: 2, StreamID: "s"},
		"negative index": {TransferID: "t", Index: -1, StreamID: "s"},
	} {
		if err := fetch(req); err == "" {
			t.Errorf("%s: fetched", name)
		}
	}
	if res := h.Call(
		weavewire.KindClipboardFetch,
		"not a request",
	); !strings.Contains(
		res.Err,
		weaveclipboard.ErrBadRequest.Error(),
	) {
		t.Errorf("an undecodable fetch: %+v", res)
	}

	// Changed or gone since it was copied: not the file that was copied.
	if err := os.WriteFile(f.Path, []byte("longer now"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fetch(
		weavewire.ClipboardFetchRequest{TransferID: "t", StreamID: "s"},
	); !strings.Contains(
		err,
		weavewire.ClipboardReasonUnreadable,
	) {
		t.Errorf("a changed file: %q", err)
	}
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := fetch(
		weavewire.ClipboardFetchRequest{TransferID: "t", Index: 1, StreamID: "s"},
	); !strings.Contains(
		err,
		weavewire.ClipboardReasonUnreadable,
	) {
		t.Errorf("a deleted file: %q", err)
	}

	// A get needs a transfer id to stream.
	if res := h.Call(
		weavewire.KindClipboardGet,
		weavewire.ClipboardGetRequest{Stream: true},
	); !strings.Contains(
		res.Err,
		weaveclipboard.ErrBadRequest.Error(),
	) {
		t.Errorf("a streaming get without a transfer id: %+v", res)
	}
}

// A newer get, or a cancel, stops what an older one is still streaming.
func TestANewerGetOrACancelStopsAStream(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, 3<<20)
	b := &fileClipboard{files: []weaveclipboard.File{writeFile(t, dir, "big.bin", big)}}
	h, _ := startFiles(t, b)
	var got weavewire.ClipboardGetResponse
	for _, tc := range []struct {
		transfer string
		stop     func()
	}{
		{"t1", func() {
			h.Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{Stream: true, TransferID: "t2"}, &got)
		}},
		{"t3", func() { h.Notify(weavewire.KindClipboardCancel, weavewire.ClipboardCancel{TransferID: "t3"}) }},
	} {
		h.Decode(
			weavewire.KindClipboardGet,
			weavewire.ClipboardGetRequest{Stream: true, TransferID: tc.transfer},
			&got,
		)
		id := "s-" + tc.transfer
		var fr weavewire.ClipboardFetchResponse
		h.Decode(
			weavewire.KindClipboardFetch,
			weavewire.ClipboardFetchRequest{TransferID: tc.transfer, StreamID: id},
			&fr,
		)
		// Wait for a window's worth, never credited, then stop it.
		h.T.WaitFor(h.Timeout, func(sent []modulesdk.Message) bool {
			return len(
				chunksOf(sent, weavewire.KindClipboardDownload, id),
			) >= weavewire.ClipboardWindowBytes/weavewire.MaxChunkBytes
		})
		tc.stop()
		if !h.T.WaitFor(h.Timeout, func(sent []modulesdk.Message) bool {
			cs := chunksOf(sent, weavewire.KindClipboardDownload, id)
			return len(cs) > 0 && cs[len(cs)-1].EOF
		}) {
			t.Fatalf("%s: the stream did not end", tc.transfer)
		}
		cs := chunksOf(h.T.Sent(), weavewire.KindClipboardDownload, id)
		if end := cs[len(cs)-1]; end.Err == "" ||
			len(cs) > weavewire.ClipboardWindowBytes/weavewire.MaxChunkBytes+1 {
			t.Errorf("%s: %d chunks, ending %+v", tc.transfer, len(cs), end)
		}
	}
	// A cancel for a transfer that is not current is nothing to do.
	h.Notify(weavewire.KindClipboardCancel, weavewire.ClipboardCancel{TransferID: "old"})
	if res := h.Call(weavewire.KindClipboardCancel, "x"); res.Err == "" {
		t.Error("an undecodable cancel")
	}
	if res := h.Call(weavewire.KindClipboardCredit, "x"); res.Err == "" {
		t.Error("an undecodable credit")
	}
}

// A streaming get's other representations stream under its transfer id,
// flow-controlled, with no ceiling in total; each is judged against the cap.
func TestStreamingGetStreamsLargeRepresentations(t *testing.T) {
	png := bytes.Repeat([]byte{7}, 3<<20)
	b := &fileClipboard{memClipboard: memClipboard{token: 1, items: []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardPNG, Data: png},
		text("caption"),
		{Format: weavewire.ClipboardHTML, Size: 5}, // a backend that left it unread
	}}}
	h, _ := startFiles(t, b)
	var got weavewire.ClipboardGetResponse
	h.Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{
		Stream: true, TransferID: "t", Formats: []weavewire.ClipboardFormat{
			weavewire.ClipboardPNG, weavewire.ClipboardText, weavewire.ClipboardHTML,
		},
	}, &got)
	if !got.Streamed || len(got.Items) != 2 || len(got.Omitted) != 1 {
		t.Fatalf("get %+v", got)
	}
	body, end := stream(t, h, "t")
	if !bytes.Equal(body, append(slices.Clone(png), "caption"...)) || end.Digest != sum(body) {
		t.Errorf("streamed %d bytes", len(body))
	}
	got = weavewire.ClipboardGetResponse{}
	h.Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{
		Stream:     true,
		TransferID: "u",
		Formats:    []weavewire.ClipboardFormat{weavewire.ClipboardText},
	}, &got)
	if got.Streamed || len(got.Items) != 1 {
		t.Errorf("a get of the text alone: %+v", got)
	}
}

// stage readies one item, failing the test on a refusal.
func stage(t *testing.T, h *weavemoduletest.Harness, req weavewire.ClipboardStageRequest) {
	t.Helper()
	var st weavewire.ClipboardStageResponse
	h.Decode(weavewire.KindClipboardStage, req, &st)
	if st.Refused != "" {
		t.Fatalf("stage %s refused: %+v", req.StreamID, st)
	}
}

// put sends data as id's chunks, its EOF carrying digest.
func put(h *weavemoduletest.Harness, id string, data []byte, digest string) {
	var seq uint64
	for len(data) > 0 {
		n := min(len(data), weavewire.MaxChunkBytes)
		h.Notify(
			weavewire.KindClipboardPut,
			weavewire.Chunk{StreamID: id, Seq: seq, Data: data[:n]},
		)
		seq++
		data = data[n:]
	}
	h.Notify(
		weavewire.KindClipboardPut,
		weavewire.Chunk{StreamID: id, Seq: seq, EOF: true, Digest: digest},
	)
}

// done waits for the guest's verdict on a staged item.
func done(t *testing.T, h *weavemoduletest.Harness, id string) weavewire.ClipboardStaged {
	t.Helper()
	var ev weavewire.ClipboardStaged
	if !h.T.WaitFor(h.Timeout, func(sent []modulesdk.Message) bool {
		for _, e := range stagedOf(sent, id) {
			if e.Done {
				ev = e
				return true
			}
		}
		return false
	}) {
		t.Fatalf("no verdict on %s", id)
	}
	return ev
}

// A set's large items stream to disk ahead of it, acknowledged as they are
// written and verified at the end; the set publishes them, files by path.
func TestStagedItemsArePublishedBySet(t *testing.T) {
	b := &fileClipboard{}
	h, root := startFiles(t, b)
	file := bytes.Repeat([]byte("f"), 700<<10)
	png := bytes.Repeat([]byte("p"), 300<<10)
	stage(
		t,
		h,
		weavewire.ClipboardStageRequest{
			TransferID: "t",
			StreamID:   "a",
			Format:     weavewire.ClipboardFiles,
			Name:       `C:\host\a.bin`,
			Size:       int64(len(file)),
		},
	)
	stage(
		t,
		h,
		weavewire.ClipboardStageRequest{
			TransferID: "t",
			StreamID:   "b",
			Format:     weavewire.ClipboardPNG,
			Size:       int64(len(png)),
		},
	)
	put(h, "a", file, sum(file))
	put(h, "b", png, sum(png))
	if ev := done(t, h, "a"); ev.Reason != "" || ev.Acked != int64(len(file)) {
		t.Fatalf("a: %+v", ev)
	}
	if acks := stagedOf(h.T.Sent(), "a"); len(acks) != 3 { // 256K, 512K, done
		t.Errorf("acknowledged %+v", acks)
	}
	if ev := done(t, h, "b"); ev.Reason != "" {
		t.Fatalf("b: %+v", ev)
	}

	var res weavewire.ClipboardSetResponse
	h.Decode(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardFiles, Name: "a.bin", Size: int64(len(file)), Stream: "a"},
			{Format: weavewire.ClipboardPNG, Size: int64(len(png)), Stream: "b"},
			{Format: weavewire.ClipboardFiles, Name: "inline.txt", Size: 6, Data: []byte("inline")},
			text("caption"),
		}},
		&res,
	)
	paths := b.lastPaths()
	if len(paths) != 2 || filepath.Base(paths[0]) != "a.bin" ||
		filepath.Base(paths[1]) != "inline.txt" {
		t.Fatalf("paths %v", paths)
	}
	if data, err := os.ReadFile(paths[0]); err != nil || !bytes.Equal(data, file) {
		t.Errorf("a.bin holds %d bytes, %v", len(data), err)
	}
	written := b.written[len(b.written)-1]
	if !bytes.Equal(written[1].Data, png) || written[0].Data != nil || written[2].Data != nil {
		t.Errorf("the backend was handed %+v", formatsAndSizes(written))
	}

	// The next set replaces the clipboard, and the files of the one before go.
	h.Decode(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardFiles, Name: "next.txt", Data: []byte("next")},
		}},
		&res,
	)
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Errorf("the replaced copy's files are still there: %v", err)
	}
	if left, _ := os.ReadDir(root); len(left) != 1 {
		t.Errorf("staging holds %v", left)
	}
	// Stopping keeps what the clipboard holds.
	next := b.lastPaths()[0]
	if err := h.Module.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(next); err != nil {
		t.Errorf("stopping removed the clipboard's file: %v", err)
	}
}

func formatsAndSizes(items []weavewire.ClipboardItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, string(it.Format)+":"+it.Name+":"+string(rune('0'+min(len(it.Data), 9))))
	}
	return out
}

func TestStageRefusals(t *testing.T) {
	h, _ := startFiles(t, &fileClipboard{})
	for name, req := range map[string]any{
		"undecodable":  "x",
		"no transfer":  weavewire.ClipboardStageRequest{StreamID: "s", Format: weavewire.ClipboardPNG},
		"no stream":    weavewire.ClipboardStageRequest{TransferID: "t", Format: weavewire.ClipboardPNG},
		"no format":    weavewire.ClipboardStageRequest{TransferID: "t", StreamID: "s"},
		"negative":     weavewire.ClipboardStageRequest{TransferID: "t", StreamID: "s", Format: weavewire.ClipboardPNG, Size: -1},
		"a bad name":   weavewire.ClipboardStageRequest{TransferID: "t", StreamID: "s", Format: weavewire.ClipboardFiles, Name: ".."},
		"no file name": weavewire.ClipboardStageRequest{TransferID: "t", StreamID: "s", Format: weavewire.ClipboardFiles},
	} {
		if res := h.Call(
			weavewire.KindClipboardStage,
			req,
		); !strings.Contains(
			res.Err,
			weaveclipboard.ErrBadRequest.Error(),
		) {
			t.Errorf("%s: %+v", name, res)
		}
	}
	stage(
		t,
		h,
		weavewire.ClipboardStageRequest{
			TransferID: "t",
			StreamID:   "s",
			Format:     weavewire.ClipboardPNG,
			Size:       1,
		},
	)
	if res := h.Call(weavewire.KindClipboardStage, weavewire.ClipboardStageRequest{
		TransferID: "t", StreamID: "s", Format: weavewire.ClipboardPNG, Size: 1,
	}); !strings.Contains(res.Err, "already staged") {
		t.Errorf("a stream staged twice: %+v", res)
	}
	if res := h.Call(weavewire.KindClipboardPut, "x"); res.Err == "" {
		t.Error("an undecodable put")
	}
}

// A guest without room for an item refuses it, with its free space, before a
// byte of it is sent; one whose disk fills as it arrives drops it.
func TestStageGuardsTheGuestDisk(t *testing.T) {
	free := int64(cliptransfer.Reserve + 100)
	old := cliptransfer.FreeBytes
	cliptransfer.FreeBytes = func(string) (int64, error) { return free, nil }
	t.Cleanup(func() { cliptransfer.FreeBytes = old })

	h, _ := startFiles(t, &fileClipboard{})
	var st weavewire.ClipboardStageResponse
	h.Decode(weavewire.KindClipboardStage, weavewire.ClipboardStageRequest{
		TransferID: "t", StreamID: "big", Format: weavewire.ClipboardFiles, Name: "big", Size: 101,
	}, &st)
	if st.Refused != weavewire.ClipboardReasonNoSpace || st.Free != free {
		t.Errorf("an item with no room: %+v", st)
	}
	stage(
		t,
		h,
		weavewire.ClipboardStageRequest{
			TransferID: "t",
			StreamID:   "fits",
			Format:     weavewire.ClipboardFiles,
			Name:       "fits",
			Size:       100,
		},
	)
	if res := h.Call(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardFiles, Name: "big", Size: 101, Stream: "big"},
		}},
	); !strings.Contains(
		res.Err,
		weaveclipboard.ErrUpload.Error(),
	) {
		t.Errorf("a set of a refused item: %+v", res)
	}
}

// What is not the item the host sent is dropped, with why, and a set that
// names it is refused.
func TestStagedItemThatIsNotWholeIsRefused(t *testing.T) {
	h, root := startFiles(t, &fileClipboard{})
	stage(
		t,
		h,
		weavewire.ClipboardStageRequest{
			TransferID: "t",
			StreamID:   "bad",
			Format:     weavewire.ClipboardFiles,
			Name:       "f",
			Size:       5,
		},
	)
	stage(
		t,
		h,
		weavewire.ClipboardStageRequest{
			TransferID: "t",
			StreamID:   "slow",
			Format:     weavewire.ClipboardFiles,
			Name:       "g",
			Size:       5,
		},
	)
	put(h, "bad", []byte("hello"), sum([]byte("other")))
	if ev := done(t, h, "bad"); ev.Reason != weavewire.ClipboardReasonIntegrity || ev.Err == "" {
		t.Errorf("a wrong digest: %+v", ev)
	}
	// Chunks after the verdict are dropped.
	h.Notify(
		weavewire.KindClipboardPut,
		weavewire.Chunk{StreamID: "bad", Seq: 2, Data: []byte("x")},
	)
	h.Notify(
		weavewire.KindClipboardPut,
		weavewire.Chunk{StreamID: "nobody", Seq: 0, Data: []byte("x")},
	)
	h.Notify(
		weavewire.KindClipboardPut,
		weavewire.Chunk{StreamID: "slow", Seq: 0, Data: []byte("he")},
	)
	for stream, want := range map[string]string{"bad": "integrity", "slow": "still streaming", "never": "nothing is staged"} {
		res := h.Call(
			weavewire.KindClipboardSet,
			weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{
				{Format: weavewire.ClipboardFiles, Name: "f", Size: 5, Stream: stream},
			}},
		)
		if !strings.Contains(res.Err, want) {
			t.Errorf("a set of %s: %+v", stream, res)
		}
	}
	h.Notify(
		weavewire.KindClipboardPut,
		weavewire.Chunk{StreamID: "slow", Seq: 1, Data: []byte("llo")},
	)
	h.Notify(
		weavewire.KindClipboardPut,
		weavewire.Chunk{StreamID: "slow", Seq: 2, EOF: true, Digest: sum([]byte("hello"))},
	)
	done(t, h, "slow")
	if res := h.Call(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardFiles, Name: "g", Size: 4, Stream: "slow"},
		}},
	); !strings.Contains(
		res.Err,
		"declares 4",
	) {
		t.Errorf("a set whose size differs from what was staged: %+v", res)
	}

	// A cancel deletes what the transfer staged; a newer transfer
	// supersedes it the same way.
	h.Notify(weavewire.KindClipboardCancel, weavewire.ClipboardCancel{TransferID: "t"})
	deadline := time.Now().Add(h.Timeout)
	for left, _ := os.ReadDir(root); len(left) != 0; left, _ = os.ReadDir(root) {
		if time.Now().After(deadline) {
			t.Fatal("a cancel left the staging behind")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stage(
		t,
		h,
		weavewire.ClipboardStageRequest{
			TransferID: "u",
			StreamID:   "x",
			Format:     weavewire.ClipboardPNG,
			Size:       9,
		},
	)
	stage(
		t,
		h,
		weavewire.ClipboardStageRequest{
			TransferID: "v",
			StreamID:   "y",
			Format:     weavewire.ClipboardPNG,
			Size:       9,
		},
	)
	if left, _ := os.ReadDir(root); len(left) != 1 {
		t.Errorf("a newer transfer left the older's staging: %v", left)
	}
	put(h, "x", []byte("superseded"), "")
	if acks := stagedOf(h.T.Sent(), "x"); len(acks) != 0 {
		t.Errorf("a superseded item was acknowledged: %+v", acks)
	}
	// A stage for a cancelled transfer — one that overtook its cancel — is
	// refused, and so is one after the module stops.
	var st weavewire.ClipboardStageResponse
	h.Decode(weavewire.KindClipboardStage, weavewire.ClipboardStageRequest{
		TransferID: "t", StreamID: "late", Format: weavewire.ClipboardPNG, Size: 1,
	}, &st)
	if st.Refused != weavewire.ClipboardReasonCancelled {
		t.Errorf("a stage after its cancel: %+v", st)
	}
	// Stopping deletes what is in flight.
	if err := h.Module.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Errorf("stopping left %v", left)
	}
}

// A backend that takes no paths is handed staged files' content.
func TestABackendWithoutPathsGetsContent(t *testing.T) {
	b := &memClipboard{}
	h := weavemoduletest.Start(
		t,
		weaveclipboard.NewService(b, weaveclipboard.WithStagingDir(t.TempDir())),
	)
	stage(
		t,
		h,
		weavewire.ClipboardStageRequest{
			TransferID: "t",
			StreamID:   "a",
			Format:     weavewire.ClipboardFiles,
			Name:       "a",
			Size:       3,
		},
	)
	put(h, "a", []byte("abc"), sum([]byte("abc")))
	done(t, h, "a")
	var res weavewire.ClipboardSetResponse
	h.Decode(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardFiles, Name: "a", Size: 3, Stream: "a"},
			{Format: weavewire.ClipboardFiles, Name: "b", Data: []byte("bb")},
		}},
		&res,
	)
	if len(b.items) != 2 || string(b.items[0].Data) != "abc" || string(b.items[1].Data) != "bb" {
		t.Errorf("the backend was handed %+v", b.items)
	}
}

// For a host that does not stream, files offered by path are read into the
// reply, within the older transfer's ceiling.
func TestAnOlderHostGetsFilesInline(t *testing.T) {
	dir := t.TempDir()
	b := &fileClipboard{files: []weaveclipboard.File{
		writeFile(t, dir, "a.txt", []byte("abc")),
		writeFile(t, dir, "empty", nil),
		writeFile(t, dir, "big.bin", make([]byte, 50)),
		{Name: "gone", Path: filepath.Join(dir, "gone"), Size: 4},
	}}
	h, _ := startFiles(t, b)
	var got weavewire.ClipboardGetResponse
	h.Decode(weavewire.KindClipboardGet, weavewire.ClipboardGetRequest{MaxBytes: 10}, &got)
	if len(got.Items) != 2 || string(got.Items[0].Data) != "abc" || got.Items[1].Name != "empty" ||
		got.Items[1].Deferred || len(got.Omitted) != 2 {
		t.Errorf("get %+v", got)
	}
}

func TestStagingFailures(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := weavemoduletest.Start(
		t,
		weaveclipboard.NewService(&fileClipboard{}, weaveclipboard.WithStagingDir(notDir)),
	)
	if res := h.Call(weavewire.KindClipboardStage, weavewire.ClipboardStageRequest{
		TransferID: "t", StreamID: "s", Format: weavewire.ClipboardPNG,
	}); res.Err == "" {
		t.Error("staged under a file")
	}
	if res := h.Call(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardFiles, Name: "a", Data: []byte("a")},
		}},
	); res.Err == "" {
		t.Error("an inline file staged under a file")
	}

	// Without a staging directory of its own, it stages in a run directory of
	// its own under the user's cache directory.
	cache := t.TempDir()
	t.Cleanup(
		weaveclipboard.UseStaging(func() (string, error) { return cache, nil }, t.TempDir(), nil),
	)
	b := &fileClipboard{}
	h = weavemoduletest.Start(t, weaveclipboard.NewService(b))
	var res weavewire.ClipboardSetResponse
	h.Decode(
		weavewire.KindClipboardSet,
		weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{
			{Format: weavewire.ClipboardFiles, Name: "a", Data: []byte("a")},
		}},
		&res,
	)
	base := filepath.Join(cache, "weave", "clipboard")
	if p := b.lastPaths(); len(p) != 1 || !strings.HasPrefix(p[0], filepath.Join(base, "run-")) {
		t.Errorf("staged at %v, want a run directory in %s", p, base)
	}

	// With nowhere to make one, staging fails, saying so.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(
		weaveclipboard.UseStaging(func() (string, error) { return blocked, nil }, blocked, nil),
	)
	setTemp(t, blocked)
	h = weavemoduletest.Start(t, weaveclipboard.NewService(b))
	if res := h.Call(weavewire.KindClipboardStage, weavewire.ClipboardStageRequest{
		TransferID: "t", StreamID: "s", Format: weavewire.ClipboardPNG,
	}); res.Err == "" {
		t.Error("staged with nowhere to stage")
	}

	// A refused name, and a backend that fails, leave nothing staged.
	root := t.TempDir()
	b = &fileClipboard{writeErr: errors.New("refused")}
	h = weavemoduletest.Start(t, weaveclipboard.NewService(b, weaveclipboard.WithStagingDir(root)))
	for _, items := range [][]weavewire.ClipboardItem{
		{{Format: weavewire.ClipboardFiles, Name: "..", Data: []byte("a")}},
		{{Format: weavewire.ClipboardFiles, Name: "a", Data: []byte("a")}},
	} {
		if res := h.Call(
			weavewire.KindClipboardSet,
			weavewire.ClipboardSetRequest{Items: items},
		); res.Err == "" {
			t.Errorf("set %+v", items)
		}
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// setTemp points the temporary directory at dir, on every OS.
func setTemp(t *testing.T, dir string) {
	t.Helper()
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, dir)
	}
}
