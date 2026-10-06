// Package weaveclipboardtest is the clipboard contract: one test, run by every
// weave-<os>-clipboard module against its real backend, so the clipboard
// behaves the same on every guest OS.
//
// The contract drives a backend through weaveclipboard's Service, over the
// module test harness, exactly as a host's calls reach it: what it checks is
// what a host sees on the wire. It covers the change token, the formats stat
// reports and what the guest says it can hold, a get's format filter and size
// cap, a set of every canonical format read back representation by
// representation, files (staged, named, capped and reported when omitted),
// content large enough to stream both ways, and the error answers, the
// unsupported code included.
//
// A module runs it against a clipboard of the test's own wherever its OS
// offers one, never the clipboard of the machine running the test: macOS
// gives each test a uniquely named pasteboard, Linux a display or compositor
// started for the test, and Windows, which has no private clipboard, a
// stand-in for the Win32 clipboard calls beneath the real backend.
package weaveclipboardtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Contract is one backend under test.
type Contract struct {
	// New returns the backend over a clipboard of the test's own, which the
	// contract writes freely. It is called once per check; anything it holds
	// open is the caller's to close with t.Cleanup.
	New func(t *testing.T) weaveclipboard.Backend
	// Unavailable, when set, returns the backend as it is with no clipboard
	// to reach (no display server, no pasteboard server). Every op must then
	// answer unsupported. Leave it nil on an OS where a session always has a
	// clipboard; the check is then skipped, saying so.
	Unavailable func(t *testing.T) weaveclipboard.Backend
}

// Sample content, one representation of each canonical format. The bytes
// carry no meaning a clipboard could reinterpret — no line endings to
// convert, no NUL inside the text formats — so every guest must hand back
// exactly what it was given.
var (
	sampleText = []byte("héllo ✓ from weave")
	sampleHTML = []byte("<b>héllo</b> <i>from weave</i>")
	sampleRTF  = []byte(`{\rtf1\ansi\deff0 {\b hello} from weave}`)
	samplePNG  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x42}, 600)...)
	sampleTIFF = append([]byte("II*\x00"), bytes.Repeat([]byte{0x24}, 300)...)
	samplePDF  = []byte(
		"%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n",
	)
)

// sample returns one item of format f.
func sample(f weavewire.ClipboardFormat) weavewire.ClipboardItem {
	data := map[weavewire.ClipboardFormat][]byte{
		weavewire.ClipboardText: sampleText,
		weavewire.ClipboardHTML: sampleHTML,
		weavewire.ClipboardRTF:  sampleRTF,
		weavewire.ClipboardPNG:  samplePNG,
		weavewire.ClipboardTIFF: sampleTIFF,
		weavewire.ClipboardPDF:  samplePDF,
	}[f]
	return weavewire.ClipboardItem{Format: f, Data: data}
}

// oneOf returns one item of format f, a file for ClipboardFiles.
func oneOf(f weavewire.ClipboardFormat) weavewire.ClipboardItem {
	if f == weavewire.ClipboardFiles {
		return files()[0]
	}
	return sample(f)
}

// files are copied files: two of the same name from different host folders
// (one named by a Windows path), so staging must keep both, and one larger
// than the size cap the checks use.
func files() []weavewire.ClipboardItem {
	return []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "notes.txt", Data: []byte("first")},
		{
			Format: weavewire.ClipboardFiles,
			Name:   `C:\Users\me\notes.txt`,
			Data:   []byte("second, larger"),
		},
		{
			Format: weavewire.ClipboardFiles,
			Name:   "/home/me/big.bin",
			Data:   bytes.Repeat([]byte{7}, 2000),
		},
	}
}

// fileCap is the size cap the checks read with: over every sample's text,
// under big.bin and the image.
const fileCap = 100

// RunContract runs every check against c.
func RunContract(t *testing.T, c Contract) {
	t.Helper()
	checks := []struct {
		name string
		run  func(t testing.TB, h *harness)
	}{
		{"Support", checkSupport},
		{"TokenChangesOnEverySet", checkToken},
		{"EveryRepresentation", checkEveryRepresentation},
		{"GetHonoursFormatsAndMaxBytes", checkGet},
		{"Files", checkFiles},
		{"FilesOverTheCapLeaveTheRest", checkFilesOverCap},
		{"LargeContentStreams", checkLarge},
		{"Errors", checkErrors},
	}
	for _, ck := range checks {
		t.Run(ck.name, func(t *testing.T) {
			ck.run(t, start(t, c.New(t)))
		})
	}
	t.Run("Unavailable", func(t *testing.T) {
		if c.Unavailable == nil {
			t.Skip("this OS has no state without a clipboard: a session always has one")
		}
		checkUnavailable(t, start(t, c.Unavailable(t)))
	})
}

// harness is the service over one backend, with what stat says it holds.
type harness struct {
	*weavemoduletest.Harness
	support weavewire.ClipboardStatResponse
	nextID  int
}

func start(t testing.TB, b weaveclipboard.Backend) *harness {
	t.Helper()
	h := &harness{Harness: weavemoduletest.Start(t, weaveclipboard.NewService(b))}
	h.Timeout = 30 * time.Second
	return h
}

// held lists the canonical formats the guest holds, richest first. It asks
// stat once.
func (h *harness) held(t testing.TB) []weavewire.ClipboardFormat {
	t.Helper()
	if h.support.Support == nil {
		h.support = h.stat(t)
	}
	var out []weavewire.ClipboardFormat
	for _, s := range h.support.Support {
		if s.Held {
			out = append(out, s.Format)
		}
	}
	return out
}

func (h *harness) single(t testing.TB) bool {
	t.Helper()
	h.held(t)
	return h.support.SingleRepresentation
}

func (h *harness) stat(t testing.TB) weavewire.ClipboardStatResponse {
	t.Helper()
	var st weavewire.ClipboardStatResponse
	h.Decode(weavewire.KindClipboardStat, nil, &st)
	return st
}

// set writes items inline, or as an upload when they are over the inline
// limit, as weaveclient does.
func (h *harness) set(t testing.TB, items []weavewire.ClipboardItem) weavewire.Result {
	t.Helper()
	req := weavewire.ClipboardSetRequest{Items: slices.Clone(items)}
	var all []byte
	for i := range req.Items {
		req.Items[i].Size = int64(len(req.Items[i].Data))
		all = append(all, req.Items[i].Data...)
	}
	if len(all) > weavewire.ClipboardInlineBytes {
		h.nextID++
		req.TransferID = fmt.Sprintf("contract-up-%d", h.nextID)
		var seq uint64
		for len(all) > 0 {
			n := min(len(all), weavewire.MaxChunkBytes)
			h.Notify(weavewire.KindClipboardUpload,
				weavewire.Chunk{StreamID: req.TransferID, Seq: seq, Data: all[:n]})
			seq++
			all = all[n:]
		}
		h.Notify(weavewire.KindClipboardUpload,
			weavewire.Chunk{StreamID: req.TransferID, Seq: seq, EOF: true})
		for i := range req.Items {
			req.Items[i].Data = nil
		}
	}
	return h.Call(weavewire.KindClipboardSet, req)
}

func (h *harness) mustSet(
	t testing.TB,
	items []weavewire.ClipboardItem,
) weavewire.ClipboardSetResponse {
	t.Helper()
	res := h.set(t, items)
	if res.Err != "" {
		t.Fatalf("set: %s", res.Err)
	}
	var out weavewire.ClipboardSetResponse
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatalf("set: decoding: %v", err)
	}
	return out
}

// get reads the clipboard, collecting a streamed reply's chunks.
func (h *harness) get(
	t testing.TB,
	req weavewire.ClipboardGetRequest,
) weavewire.ClipboardGetResponse {
	t.Helper()
	h.nextID++
	req.TransferID = fmt.Sprintf("contract-down-%d", h.nextID)
	var got weavewire.ClipboardGetResponse
	h.Decode(weavewire.KindClipboardGet, req, &got)
	if !got.Streamed {
		return got
	}
	body := h.download(t, req.TransferID)
	for i := range got.Items {
		n := got.Items[i].Size
		if int64(len(body)) < n {
			t.Fatalf("the stream ended %d bytes into %s's %d", len(body), got.Items[i].Format, n)
		}
		got.Items[i].Data, body = body[:n:n], body[n:]
	}
	if len(body) != 0 {
		t.Fatalf("the stream carried %d bytes past the items", len(body))
	}
	return got
}

// download waits for a stream's EOF and reassembles it.
func (h *harness) download(t testing.TB, id string) []byte {
	t.Helper()
	chunks := func(sent []modulesdk.Message) []weavewire.Chunk {
		var out []weavewire.Chunk
		for _, m := range sent {
			var c weavewire.Chunk
			if m.Kind == weavewire.KindClipboardDownload && json.Unmarshal(m.Data, &c) == nil &&
				c.StreamID == id {
				out = append(out, c)
			}
		}
		return out
	}
	if !h.T.WaitFor(h.Timeout, func(sent []modulesdk.Message) bool {
		return slices.ContainsFunc(chunks(sent), func(c weavewire.Chunk) bool { return c.EOF })
	}) {
		t.Fatalf("the download %s never ended", id)
	}
	var (
		asm  weavewire.StreamAssembler
		body []byte
	)
	for _, c := range chunks(h.T.Sent()) {
		data, err := asm.Accept(c)
		if err != nil {
			t.Fatalf("download %s: %v", id, err)
		}
		body = append(body, data...)
	}
	if !asm.Done() || asm.Err() != nil {
		t.Fatalf("download %s: done %v, %v", id, asm.Done(), asm.Err())
	}
	return body
}

// Stat says, for every canonical format in order, whether the guest holds it
// and under which native name, or why not.
func checkSupport(t testing.TB, h *harness) {
	st := h.stat(t)
	formats := weavewire.ClipboardFormats()
	if len(st.Support) != len(formats) {
		t.Fatalf(
			"support lists %d formats, want every canonical one: %+v",
			len(st.Support),
			st.Support,
		)
	}
	for i, s := range st.Support {
		switch {
		case s.Format != formats[i]:
			t.Errorf("support[%d] is %s, want %s", i, s.Format, formats[i])
		case s.Held && s.Native == "":
			t.Errorf("%s is held under no native name", s.Format)
		case !s.Held && s.Reason == "":
			t.Errorf("%s is not held, and no reason is given", s.Format)
		case !s.Held && s.Private:
			t.Errorf("%s is private but not held", s.Format)
		}
	}
	if st.SingleRepresentation && st.Limitation == "" {
		t.Error("a single-representation clipboard gives no limitation")
	}
	if len(h.held(t)) == 0 {
		t.Fatal("the guest holds no format at all")
	}
}

// Every set moves the token, a set of exactly what the clipboard holds
// included, and stat and get report the token of what is there.
func checkToken(t testing.TB, h *harness) {
	text := []weavewire.ClipboardItem{sample(weavewire.ClipboardText)}
	if !slices.Contains(h.held(t), weavewire.ClipboardText) {
		text = []weavewire.ClipboardItem{oneOf(h.held(t)[0])}
	}
	before := h.stat(t).ChangeToken
	seen := make([]uint64, 1, 4)
	seen[0] = before
	for i := range 3 {
		res := h.mustSet(t, text)
		if slices.Contains(seen, res.ChangeToken) {
			t.Fatalf("set %d: token %d was seen before (%v)", i+1, res.ChangeToken, seen)
		}
		seen = append(seen, res.ChangeToken)
		if st := h.stat(t); st.ChangeToken != res.ChangeToken {
			t.Fatalf("set %d: token %d, stat afterwards %d", i+1, res.ChangeToken, st.ChangeToken)
		}
		if got := h.get(t, weavewire.ClipboardGetRequest{}); got.ChangeToken != res.ChangeToken {
			t.Fatalf("set %d: token %d, get afterwards %d", i+1, res.ChangeToken, got.ChangeToken)
		}
	}
	// Reading changes nothing.
	if again := h.stat(t).ChangeToken; again != seen[len(seen)-1] {
		t.Errorf("stat after reads: token %d, want %d", again, seen[len(seen)-1])
	}
}

// A set of every canonical format writes each one the guest holds — or the
// richest, on a single-representation clipboard — lists every other one as
// unwritten, and each written one reads back exactly.
func checkEveryRepresentation(t testing.TB, h *harness) {
	held := h.held(t)
	var items []weavewire.ClipboardItem
	for _, f := range weavewire.ClipboardFormats() {
		if f == weavewire.ClipboardFiles {
			items = append(items, files()[:2]...)
		} else {
			items = append(items, sample(f))
		}
	}
	res := h.mustSet(t, items)

	want := held
	if h.single(t) {
		want = held[:1] // ClipboardFormats is richest first
	}
	if !sameSet(res.Written, want) {
		t.Fatalf("written %v, want %v", res.Written, want)
	}
	var unwritten []weavewire.ClipboardFormat
	for _, f := range weavewire.ClipboardFormats() {
		if !slices.Contains(want, f) {
			unwritten = append(unwritten, f)
		}
	}
	if !sameSet(res.Unwritten, unwritten) {
		t.Errorf("unwritten %v, want %v", res.Unwritten, unwritten)
	}

	st := h.stat(t)
	for _, f := range want {
		i := slices.IndexFunc(
			st.Formats,
			func(fi weavewire.ClipboardFormatInfo) bool { return fi.Format == f },
		)
		if i < 0 {
			t.Errorf("stat does not list %s: %+v", f, st.Formats)
			continue
		}
		if f == weavewire.ClipboardFiles && (st.Formats[i].Count != 2 || st.Formats[i].Size != 19) {
			t.Errorf("stat reports files as %+v, want 2 files of 19 bytes", st.Formats[i])
		}
		// A host auditing a copy it does not read has only these sizes.
		n := int64(len(sample(f).Data))
		if f != weavewire.ClipboardFiles && st.Formats[i].Size != n {
			t.Errorf("stat sizes %s at %d bytes, want %d", f, st.Formats[i].Size, n)
		}
	}

	got := h.get(t, weavewire.ClipboardGetRequest{Formats: want})
	if len(got.Omitted) != 0 {
		t.Errorf("omitted %+v with no cap", got.Omitted)
	}
	var gotFiles []weavewire.ClipboardItem
	for _, f := range want {
		var reads []weavewire.ClipboardItem
		for _, it := range got.Items {
			if it.Format == f {
				reads = append(reads, it)
			}
		}
		if f == weavewire.ClipboardFiles {
			gotFiles = reads
			continue
		}
		if len(reads) != 1 || !bytes.Equal(reads[0].Data, sample(f).Data) {
			t.Errorf("%s read back as %q, want %q", f, dataOf(reads), sample(f).Data)
		}
	}
	if slices.Contains(want, weavewire.ClipboardFiles) {
		checkFileItems(t, gotFiles, files()[:2])
	}
}

// A get returns only the formats asked for, and leaves out — listing, never
// truncating — whatever is over its cap.
func checkGet(t testing.TB, h *harness) {
	var items []weavewire.ClipboardItem
	for _, f := range []weavewire.ClipboardFormat{weavewire.ClipboardPNG, weavewire.ClipboardText} {
		if slices.Contains(h.held(t), f) {
			items = append(items, sample(f))
		}
	}
	if len(items) == 0 {
		items = append(items, oneOf(h.held(t)[0]))
	}
	sent := make(map[weavewire.ClipboardFormat][]byte)
	for _, it := range items {
		sent[it.Format] = it.Data
	}
	res := h.mustSet(t, items)

	for _, f := range res.Written {
		got := h.get(t, weavewire.ClipboardGetRequest{Formats: []weavewire.ClipboardFormat{f}})
		if len(got.Items) != 1 || got.Items[0].Format != f || len(got.Omitted) != 0 ||
			!bytes.Equal(got.Items[0].Data, sent[f]) {
			t.Errorf("a get of %s returned %v (%q), omitted %+v",
				f, formatsOf(got.Items), dataOf(got.Items), got.Omitted)
		}
	}
	none := h.get(
		t,
		weavewire.ClipboardGetRequest{Formats: []weavewire.ClipboardFormat{"x/nothing"}},
	)
	if len(none.Items) != 0 || len(none.Omitted) != 0 {
		t.Errorf("a get of a format nobody wrote returned %v", formatsOf(none.Items))
	}

	capped := h.get(t, weavewire.ClipboardGetRequest{Formats: res.Written, MaxBytes: fileCap})
	for _, f := range res.Written {
		size := int64(len(sent[f]))
		inItems := slices.ContainsFunc(
			capped.Items,
			func(it weavewire.ClipboardItem) bool { return it.Format == f },
		)
		i := slices.IndexFunc(
			capped.Omitted,
			func(it weavewire.ClipboardItem) bool { return it.Format == f },
		)
		switch {
		case size <= fileCap && !inItems:
			t.Errorf("%s (%d bytes) is under the cap and was not returned", f, size)
		case size > fileCap && (inItems || i < 0 || capped.Omitted[i].Size != size || capped.Omitted[i].Data != nil):
			t.Errorf("%s (%d bytes) over the cap is not listed as omitted with its size: %+v",
				f, size, capped.Omitted)
		}
	}
}

// Files cross as their content: staged as real files the guest's clipboard
// names by path, read back under their base names, a file over the cap
// listed with its size, and a name that could escape the staging directory
// refused without touching the clipboard.
func checkFiles(t testing.TB, h *harness) {
	if !slices.Contains(h.held(t), weavewire.ClipboardFiles) {
		t.Skip("this guest does not hold files")
	}
	all := files()
	res := h.mustSet(t, append(all, sample(weavewire.ClipboardText)))
	if !slices.Contains(res.Written, weavewire.ClipboardFiles) {
		t.Fatalf("written %v, want files", res.Written)
	}
	st := h.stat(t)
	i := slices.IndexFunc(st.Formats, func(fi weavewire.ClipboardFormatInfo) bool {
		return fi.Format == weavewire.ClipboardFiles
	})
	if i < 0 || st.Formats[i].Count != 3 || st.Formats[i].Size != 2019 {
		t.Fatalf("stat %+v, want 3 files of 2019 bytes", st.Formats)
	}

	got := h.get(t, weavewire.ClipboardGetRequest{
		Formats: []weavewire.ClipboardFormat{weavewire.ClipboardFiles}, MaxBytes: fileCap,
	})
	checkFileItems(t, got.Items, all[:2])
	if len(got.Omitted) != 1 || got.Omitted[0].Name != "big.bin" || got.Omitted[0].Size != 2000 ||
		got.Omitted[0].Data != nil || got.Omitted[0].Format != weavewire.ClipboardFiles {
		t.Errorf("omitted %+v, want big.bin sized 2000 with no data", got.Omitted)
	}
	whole := h.get(
		t,
		weavewire.ClipboardGetRequest{
			Formats: []weavewire.ClipboardFormat{weavewire.ClipboardFiles},
		},
	)
	checkFileItems(t, whole.Items, all)

	for _, bad := range []string{"..", "", "/"} {
		res := h.set(
			t,
			[]weavewire.ClipboardItem{
				{Format: weavewire.ClipboardFiles, Name: bad, Data: []byte("x")},
			},
		)
		if res.Err == "" || res.Code != "" {
			t.Errorf("a file named %q: result %+v, want a refusal", bad, res)
		}
	}
	if after := h.stat(t); after.ChangeToken != st.ChangeToken {
		t.Errorf("a refused set moved the token from %d to %d", st.ChangeToken, after.ChangeToken)
	}
}

// A copy of files of which some are over the cap crosses without them: each
// over-cap file is listed as omitted with its size, and every other file
// crosses, whichever order they were copied in. The sizes are a host's: a cap
// above the inline limit, files over it on either side of a 16-byte one, and
// enough under it that the rest streams.
func checkFilesOverCap(t testing.TB, h *harness) {
	if !slices.Contains(h.held(t), weavewire.ClipboardFiles) {
		t.Skip("this guest does not hold files")
	}
	const limit = weavewire.ClipboardInlineBytes + 64<<10
	file := func(name string, n int, b byte) weavewire.ClipboardItem {
		return weavewire.ClipboardItem{
			Format: weavewire.ClipboardFiles, Name: name, Data: bytes.Repeat([]byte{b}, n),
		}
	}
	over := file("over.iso", limit+1, 'o')
	small := file("small.txt", 16, 's')
	mid := file("mid.bin", 200<<10, 'm')
	last := file("last.bin", 2*limit, 'l')
	all := []weavewire.ClipboardItem{over, small, mid, file("mid2.bin", 200<<10, 'n'), last}

	// The set has no cap: a host sends what its own policy let through, and
	// every file it sends is staged.
	if res := h.mustSet(t, all); !slices.Contains(res.Written, weavewire.ClipboardFiles) {
		t.Fatalf("written %v, want files", res.Written)
	}
	for _, req := range []weavewire.ClipboardGetRequest{
		{Formats: []weavewire.ClipboardFormat{weavewire.ClipboardFiles}, MaxBytes: limit},
		{Formats: weavewire.ClipboardFormats(), MaxBytes: limit}, // as a host asks
	} {
		got := h.get(t, req)
		var files []weavewire.ClipboardItem
		for _, it := range got.Items {
			if it.Format == weavewire.ClipboardFiles {
				files = append(files, it)
			}
		}
		checkFileItems(t, files, all[1:4])
		if !got.Streamed {
			t.Errorf("%d bytes of files came back inline", len(dataOf(files)))
		}
		var omitted []string
		for _, it := range got.Omitted {
			if it.Format != weavewire.ClipboardFiles || it.Data != nil {
				t.Errorf("omitted %s %q carries data or the wrong format", it.Format, it.Name)
			}
			omitted = append(omitted, fmt.Sprintf("%s:%d", it.Name, it.Size))
		}
		if want := []string{
			fmt.Sprintf("over.iso:%d", len(over.Data)), fmt.Sprintf("last.bin:%d", len(last.Data)),
		}; !slices.Equal(omitted, want) {
			t.Errorf("omitted %v, want %v", omitted, want)
		}
	}

	// One file over the cap, alone with a small one, as a desktop copy.
	h.mustSet(t, all[:2])
	got := h.get(t, weavewire.ClipboardGetRequest{
		Formats: weavewire.ClipboardFormats(), MaxBytes: limit,
	})
	checkFileItems(t, got.Items, all[1:2])
	if len(got.Omitted) != 1 || got.Omitted[0].Name != "over.iso" {
		t.Errorf("omitted %+v, want over.iso alone", got.Omitted)
	}
}

// checkFileItems compares files read back with the files sent: base names,
// contents and order.
func checkFileItems(t testing.TB, got, sent []weavewire.ClipboardItem) {
	t.Helper()
	if len(got) != len(sent) {
		t.Fatalf("read %d files (%v), want %d", len(got), namesOf(got), len(sent))
	}
	for i, want := range sent {
		it := got[i]
		name := want.Name[strings.LastIndexAny(want.Name, `/\`)+1:]
		if it.Format != weavewire.ClipboardFiles || it.Name != name ||
			!bytes.Equal(it.Data, want.Data) {
			t.Errorf("file %d read back as %s %q (%d bytes), want %s (%d bytes)",
				i, it.Format, it.Name, len(it.Data), name, len(want.Data))
		}
	}
}

// Content over the inline limit is uploaded for a set and streamed back for a
// get, and arrives whole both ways.
func checkLarge(t testing.TB, h *harness) {
	f := weavewire.ClipboardPNG
	if !slices.Contains(h.held(t), f) {
		f = h.held(t)[0]
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), (1<<20)/16) // 1 MiB
	if f == weavewire.ClipboardText || f == weavewire.ClipboardHTML || f == weavewire.ClipboardRTF {
		big = bytes.Repeat([]byte("weave "), (1<<20)/6)
	}
	item := weavewire.ClipboardItem{Format: f, Data: big}
	if f == weavewire.ClipboardFiles {
		item.Name = "large.bin"
	}
	res := h.mustSet(t, []weavewire.ClipboardItem{item})
	if !slices.Equal(res.Written, []weavewire.ClipboardFormat{f}) {
		t.Fatalf("written %v, want %s", res.Written, f)
	}
	got := h.get(t, weavewire.ClipboardGetRequest{Formats: []weavewire.ClipboardFormat{f}})
	if !got.Streamed || len(got.Items) != 1 || !bytes.Equal(got.Items[0].Data, big) {
		t.Fatalf("read back streamed %v, %d items, %d bytes; want %d bytes",
			got.Streamed, len(got.Items), len(dataOf(got.Items)), len(big))
	}
}

// The error answers: malformed and oversized requests are refused, a set of
// nothing the guest holds is unsupported and leaves the clipboard alone, and
// a format the guest does not hold beside one it does is listed as unwritten.
func checkErrors(t testing.TB, h *harness) {
	held := sample(h.held(t)[0])
	if held.Format == weavewire.ClipboardFiles {
		held = files()[0]
	}
	before := h.mustSet(t, []weavewire.ClipboardItem{held})

	for name, c := range map[string]struct {
		req  any
		want error
	}{
		"no items": {weavewire.ClipboardSetRequest{}, weaveclipboard.ErrBadRequest},
		"no format": {
			weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{{Data: []byte("x")}}},
			weaveclipboard.ErrBadRequest,
		},
		"inline over the limit": {
			weavewire.ClipboardSetRequest{Items: []weavewire.ClipboardItem{{
				Format: held.Format, Data: make([]byte, weavewire.ClipboardInlineBytes+1),
			}}},
			weaveclipboard.ErrTooLarge,
		},
		"upload never sent": {
			weavewire.ClipboardSetRequest{TransferID: "absent", Items: []weavewire.ClipboardItem{
				{Format: held.Format, Size: 4},
			}},
			weaveclipboard.ErrUpload,
		},
	} {
		res := h.Call(weavewire.KindClipboardSet, c.req)
		if !strings.Contains(res.Err, c.want.Error()) || res.Code != "" {
			t.Errorf("%s: result %+v, want %q with no code", name, res, c.want)
		}
	}

	unknown := h.set(t, []weavewire.ClipboardItem{
		{Format: "application/x-weave-contract", Data: []byte("?")},
		{Format: "image/x-nothing", Data: []byte("?")},
	})
	if unknown.Code != weavewire.CodeUnsupported {
		t.Errorf(
			"a set of nothing held: result %+v, want code %s",
			unknown,
			weavewire.CodeUnsupported,
		)
	}
	if st := h.stat(t); st.ChangeToken != before.ChangeToken {
		t.Errorf(
			"an unsupported set moved the token from %d to %d",
			before.ChangeToken,
			st.ChangeToken,
		)
	}
	if got := h.get(
		t,
		weavewire.ClipboardGetRequest{Formats: []weavewire.ClipboardFormat{held.Format}},
	); len(
		got.Items,
	) == 0 {
		t.Errorf("an unsupported set emptied the clipboard")
	}

	mixed := h.mustSet(t, []weavewire.ClipboardItem{
		{Format: "application/x-weave-contract", Data: []byte("?")}, held,
	})
	if !slices.Equal(mixed.Written, []weavewire.ClipboardFormat{held.Format}) ||
		!slices.Equal(
			mixed.Unwritten,
			[]weavewire.ClipboardFormat{"application/x-weave-contract"},
		) {
		t.Errorf("a set beside an unknown format: %+v", mixed)
	}
}

// With no clipboard to reach, every op is an unsupported answer.
func checkUnavailable(t testing.TB, h *harness) {
	for kind, payload := range map[string]any{
		weavewire.KindClipboardStat: nil,
		weavewire.KindClipboardGet:  weavewire.ClipboardGetRequest{},
		weavewire.KindClipboardSet: weavewire.ClipboardSetRequest{
			Items: []weavewire.ClipboardItem{sample(weavewire.ClipboardText)},
		},
	} {
		if res := h.Call(kind, payload); res.Code != weavewire.CodeUnsupported || res.Err == "" {
			t.Errorf("%s: result %+v, want code %s", kind, res, weavewire.CodeUnsupported)
		}
	}
}

func sameSet(a, b []weavewire.ClipboardFormat) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func formatsOf(items []weavewire.ClipboardItem) []weavewire.ClipboardFormat {
	out := make([]weavewire.ClipboardFormat, 0, len(items))
	for _, it := range items {
		out = append(out, it.Format)
	}
	return out
}

func namesOf(items []weavewire.ClipboardItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func dataOf(items []weavewire.ClipboardItem) []byte {
	var out []byte
	for _, it := range items {
		out = append(out, it.Data...)
	}
	return out
}
