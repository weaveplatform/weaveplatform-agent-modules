//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

func TestWriteThenStatAndRead(t *testing.T) {
	c, f := backend(t)
	ctx := context.Background()

	res, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("héllo ✓")},
		{Format: weavewire.ClipboardHTML, Data: []byte("<b>héllo</b>")},
		{Format: weavewire.ClipboardRTF, Data: []byte(`{\rtf1 hi}`)},
		{Format: weavewire.ClipboardPNG, Data: []byte("\x89PNG")},
		{Format: weavewire.ClipboardTIFF, Data: []byte("II*\x00")},
		{Format: weavewire.ClipboardPDF, Data: []byte("%PDF")},
		{Format: weavewire.ClipboardText, Data: []byte("a second text is not written")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []weavewire.ClipboardFormat{
		weavewire.ClipboardText, weavewire.ClipboardHTML, weavewire.ClipboardRTF,
		weavewire.ClipboardPNG, weavewire.ClipboardTIFF, weavewire.ClipboardPDF,
	}; !slices.Equal(res.Written, want) {
		t.Errorf("written %v, want %v", res.Written, want)
	}
	if got := f.held(t, c.formatID(weavewire.ClipboardPDF)); string(got) != "%PDF" {
		t.Errorf("Portable Document Format holds %q", got)
	}
	if res.ChangeToken != uint64(f.seq) || f.open {
		t.Errorf("token %d, sequence %d, left open %v", res.ChangeToken, f.seq, f.open)
	}
	if got := f.held(t, cfUnicodeText); !strings.HasPrefix(string(got), "h\x00\xe9\x00") {
		t.Errorf("CF_UNICODETEXT holds % x", got)
	}

	st, err := c.Stat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []weavewire.ClipboardFormatInfo{
		{Format: weavewire.ClipboardPNG},
		{Format: weavewire.ClipboardTIFF},
		{Format: weavewire.ClipboardPDF},
		{Format: weavewire.ClipboardRTF},
		{Format: weavewire.ClipboardHTML},
		{Format: weavewire.ClipboardText},
	}; !slices.Equal(st.Formats, want) || st.ChangeToken != res.ChangeToken {
		t.Errorf("stat %+v, want %+v at %d", st, want, res.ChangeToken)
	}

	got, err := c.Read(ctx, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	read := map[weavewire.ClipboardFormat]string{}
	for _, it := range got.Items {
		read[it.Format] = string(it.Data)
	}
	for f, want := range map[weavewire.ClipboardFormat]string{
		weavewire.ClipboardText: "héllo ✓", weavewire.ClipboardHTML: "<b>héllo</b>",
		weavewire.ClipboardRTF: `{\rtf1 hi}`, weavewire.ClipboardPNG: "\x89PNG", weavewire.ClipboardTIFF: "II*\x00",
		weavewire.ClipboardPDF: "%PDF",
	} {
		if read[f] != want {
			t.Errorf("%s read back %q, want %q", f, read[f], want)
		}
	}

	text, err := c.Read(ctx, []weavewire.ClipboardFormat{weavewire.ClipboardText}, 1<<20)
	if err != nil || len(text.Items) != 1 {
		t.Errorf("text only: %+v, %v", text.Items, err)
	}
}

func TestFilesRoundTripThroughCFHDrop(t *testing.T) {
	c, _ := backend(t)
	ctx := context.Background()
	res, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "a.txt", Data: []byte("first")},
		{
			Format: weavewire.ClipboardFiles,
			Name:   `C:\elsewhere\a.txt`,
			Data:   []byte("second, larger"),
		},
	})
	if err != nil ||
		!slices.Equal(res.Written, []weavewire.ClipboardFormat{weavewire.ClipboardFiles}) {
		t.Fatalf("write %+v, %v", res, err)
	}
	st, err := c.Stat(ctx)
	if err != nil || !slices.Equal(st.Formats, []weavewire.ClipboardFormatInfo{
		{Format: weavewire.ClipboardFiles, Size: 19, Count: 2},
	}) {
		t.Errorf("stat %+v, %v", st, err)
	}
	got, err := c.Read(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 0 || len(got.Files) != 2 || got.Files[0].Name != "a.txt" ||
		got.Files[0].Size != 5 || got.Files[1].Name != "a.txt" || got.Files[1].Size != 14 {
		t.Fatalf("read %+v: want both files offered by path, sized and unread", got)
	}
	for i, want := range []string{"first", "second, larger"} {
		if data, err := os.ReadFile(got.Files[i].Path); err != nil || string(data) != want {
			t.Errorf("file %d holds %q, %v; want %q", i, data, err, want)
		}
	}

	// Files gone from disk are nothing to copy.
	if err := os.RemoveAll(c.stage.root); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Stat(ctx); len(st.Formats) != 0 {
		t.Errorf("stat after the files went: %+v", st)
	}
}

func TestReadsWhatAnotherApplicationCopied(t *testing.T) {
	c, f := backend(t)
	html := []byte("Version:0.9\r\nStartHTML:-1\r\nEndHTML:-1\r\nStartFragment:0000000089\r\n" +
		"EndFragment:0000000098\r\n<i>hi</i>\x00\x00")
	f.put(t, c.formatID(weavewire.ClipboardHTML), html)
	f.put(t, cfUnicodeText, []byte("h\x00i\x00\x00\x00junk"))
	f.put(t, c.formatID(weavewire.ClipboardRTF), []byte{})       // empty: nothing to return
	f.put(t, c.formatID(weavewire.ClipboardPNG), []byte("\x89")) // fails to render
	f.failGet = c.formatID(weavewire.ClipboardPNG)
	f.put(t, 49999, []byte("a format nobody maps"))

	got, err := c.Read(context.Background(), nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var items []string
	for _, it := range got.Items {
		items = append(items, string(it.Format)+"="+string(it.Data))
	}
	if want := []string{"text/html=<i>hi</i>", "text/plain=hi"}; !slices.Equal(items, want) {
		t.Errorf("read %q, want %q", items, want)
	}
}

func TestOpenIsRetriedWhileAnotherProcessHoldsIt(t *testing.T) {
	c, f := backend(t)
	f.busy = 3
	if _, err := c.Stat(context.Background()); err != nil {
		t.Fatalf("stat after a brief hold: %v", err)
	}
	f.busy = openAttempts
	if _, err := c.Stat(context.Background()); !errors.Is(err, errHeld) {
		t.Fatalf("err = %v, want errHeld", err)
	}
}

func TestWriteReportsEachFailure(t *testing.T) {
	ctx := context.Background()
	text := []weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Data: []byte("x")}}

	c, f := backend(t)
	f.failEmpty = true
	if _, err := c.Write(ctx, text); !errors.Is(err, errFake) {
		t.Errorf("empty failure: %v", err)
	}

	c, f = backend(t)
	f.failSet = cfUnicodeText
	if _, err := c.Write(ctx, text); !errors.Is(err, errFake) {
		t.Errorf("set failure: %v", err)
	}

	c, _ = backend(t)
	if _, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "..", Data: []byte("x")},
	}); !errors.Is(err, errBadName) {
		t.Errorf("bad file name: %v", err)
	}

	c, f = backend(t)
	f.busy = openAttempts
	if _, err := c.Write(ctx, text); !errors.Is(err, errHeld) {
		t.Errorf("held clipboard: %v", err)
	}
}

// A name that will not register is a format this session cannot hold: left
// out, never written under format 0.
func TestAnUnregisteredFormatIsLeftOut(t *testing.T) {
	c, f := backend(t)
	f.noRegs = []string{"HTML Format"}
	res, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardHTML, Data: []byte("<b>x</b>")},
		{Format: weavewire.ClipboardText, Data: []byte("x")},
	})
	if err != nil ||
		!slices.Equal(res.Written, []weavewire.ClipboardFormat{weavewire.ClipboardText}) {
		t.Fatalf("write %+v, %v", res, err)
	}
	if _, ok := f.blocks[0]; ok {
		t.Error("something was written as format 0")
	}
	// Registered once, then cached.
	id := c.formatID(weavewire.ClipboardPNG)
	f.regs["PNG"] = 1
	if again := c.formatID(weavewire.ClipboardPNG); again != id {
		t.Errorf("PNG registered as %d then %d", id, again)
	}
}

func TestDropFilesIsWhatDragQueryFileReads(t *testing.T) {
	paths := []string{`C:\Users\me\AppData\Local\Temp\weave-clipboard-1\0\a b.txt`, `C:\x\ü.txt`}
	h, err := newBlock(dropFiles(paths))
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeWin(t)
	f.blocks[cfHDrop] = h
	f.open = true
	c := &clipboard{w: f.win(t)}
	if got := c.files(); !slices.Equal(got, paths) {
		t.Errorf("DragQueryFile read %q, want %q", got, paths)
	}
}

func TestCFHTML(t *testing.T) {
	// Captured from Edge and Word copies.
	edge := "Version:0.9\r\nStartHTML:0000000105\r\nEndHTML:0000000193\r\n" +
		"StartFragment:0000000141\r\nEndFragment:0000000163\r\n" +
		"<html>\r\n<body>\r\n<!--StartFragment--><b>bold</b> text<!--EndFragment-->\r\n</body>\r\n</html>"
	doc := "<html>\r\n<body>\r\n<!--StartFragment--><b>bold</b> text<!--EndFragment-->\r\n</body>\r\n</html>"
	// cfhtml builds a block around body with the given header line ending and
	// the fragment offsets shifted by skew bytes from where they belong.
	cfhtml := func(eol string, startSkew, endSkew int, body string) string {
		const h = "Version:0.9%[1]sStartHTML:%010[2]d%[1]sEndHTML:%010[3]d%[1]s" +
			"StartFragment:%010[4]d%[1]sEndFragment:%010[5]d%[1]s"
		n := len(fmt.Sprintf(h, eol, 0, 0, 0, 0))
		fs := n + strings.Index(body, "<!--StartFragment-->") + len("<!--StartFragment-->")
		fe := n + strings.LastIndex(body, "<!--EndFragment-->")
		return fmt.Sprintf(h, eol, n, n+len(body), fs+startSkew, fe+endSkew) + body
	}
	for name, tc := range map[string]struct{ in, want string }{
		// Its EndFragment is 6 bytes past the end marker, as captured.
		"edge, offsets past the marker": {edge, "<b>bold</b> text"},
		"crlf header":                   {cfhtml("\r\n", 0, 0, doc), "<b>bold</b> text"},
		"lf header":                     {cfhtml("\n", 0, 0, doc), "<b>bold</b> text"},
		"end off by +3":                 {cfhtml("\r\n", 0, 3, doc), "<b>bold</b> text"},
		"end off by -2":                 {cfhtml("\n", 0, -2, doc), "<b>bold</b> text"},
		"start off by -4":               {cfhtml("\r\n", -4, 0, doc), "<b>bold</b> text"},
		"multibyte fragment, off by +1": {cfhtml("\r\n", 0, 1, "<p><!--StartFragment-->ü ✓<!--EndFragment--></p>"), "ü ✓"},
		"fragment past the end": {
			"Version:0.9\r\nStartHTML:0000000105\r\nEndHTML:0000000193\r\n" +
				"StartFragment:0000000141\r\nEndFragment:0000099999\r\n" + doc,
			"<b>bold</b> text",
		},
		"source url": {
			"Version:1.0\r\nStartHTML:-1\r\nEndHTML:-1\r\nStartFragment:0000000121\r\nEndFragment:0000000129\r\n" +
				"SourceURL:https://example.com/\r\n<i>x</i>\x00",
			"<i>x</i>",
		},
		"markers, no offsets": {"Version:0.9\r\n<html><!--StartFragment--><u>u</u><!--EndFragment--></html>", "<u>u</u>"},
		"document, no fragment": {
			"Version:0.9\r\nStartHTML:0000000055\r\nEndHTML:0000000066\r\n<p>kept</p>", "<p>kept</p>",
		},
		"offsets past the end": {
			"Version:0.9\r\nStartHTML:0000000999\r\nEndHTML:0000009999\r\n<p>kept</p>", "<p>kept</p>",
		},
		"fragment quoting the end marker": {
			cfhtml("\r\n", 0, 0, "<!--StartFragment-->a<!--EndFragment-->b<!--EndFragment-->"),
			"a<!--EndFragment-->b",
		},
		"start marker only": {"Version:0.9\r\n<!--StartFragment--><p>x</p>", "<!--StartFragment--><p>x</p>"},
		"header only":       {"Version:0.9\r\nStartHTML:junk\r\n", ""},
		"bare html":         {"<p>no header</p>\x00", "<p>no header</p>"},
		"negative span":     {"Version:0.9\r\nStartHTML:50\r\nEndHTML:10\r\n<a>", "<a>"},
	} {
		if got := fromCFHTML([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}

	// What toCFHTML writes, fromCFHTML reads back as the fragment it was
	// given, and its fragment offsets point exactly at the fragment.
	block := string(toCFHTML("<b>ü</b>"))
	h := header(block)
	if frag := block[h["StartFragment"]:h["EndFragment"]]; frag != "<b>ü</b>" {
		t.Errorf("fragment offsets select %q", frag)
	}
	if got := fromCFHTML([]byte(block)); got != "<b>ü</b>" {
		t.Errorf("read back %q", got)
	}
	if !strings.HasSuffix(block, "</html>\x00") ||
		block[h["StartHTML"]:h["EndHTML"]] != fragmentStart+"<b>ü</b>"+fragmentEnd {
		t.Errorf("block %q", block)
	}
}

func TestTextEncodings(t *testing.T) {
	for _, s := range []string{"", "plain", "héllo ✓", "emoji 😀"} {
		if got := decodeUTF16(encodeUTF16(s)); got != s {
			t.Errorf("round trip %q → %q", s, got)
		}
	}
	if got := decodeUTF16([]byte("a\x00b")); got != "a" {
		t.Errorf("odd-length block read as %q", got)
	}
	if got := string(trimNUL([]byte("rtf\x00\x00pad"))); got != "rtf" {
		t.Errorf("trimNUL = %q", got)
	}
	if got := string(encode(weavewire.ClipboardRTF, []byte("r"))); got != "r\x00" {
		t.Errorf("RTF block %q", got)
	}
	if got := string(decode(weavewire.ClipboardPNG, []byte("raw"))); got != "raw" {
		t.Errorf("PNG decoded as %q", got)
	}
}

// The real clipboard, read only: the backend reaches this session's
// clipboard. Nothing is written. A runner without an interactive desktop has
// no clipboard to open, which is a skip, not a failure.
func TestStatOfTheRealClipboard(t *testing.T) {
	c := newClipboard()
	st, err := c.Stat(context.Background())
	if errors.Is(err, errHeld) {
		t.Skipf("the clipboard cannot be opened here: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(
		context.Background(),
		[]weavewire.ClipboardFormat{weavewire.ClipboardText},
		1<<10,
	); err != nil {
		t.Fatal(err)
	}
	t.Logf("sequence %d, %d formats", st.ChangeToken, len(st.Formats))
	if id := c.formatID(weavewire.ClipboardHTML); id < 0xC000 {
		t.Errorf("HTML Format registered as %d, want a registered format number", id)
	}
	if got := filepath.Base(`C:\a\b.txt`); got != "b.txt" {
		t.Errorf("filepath.Base = %q", got)
	}
}

// The real write calls, where they cannot succeed: without the clipboard open
// on the calling thread Windows refuses both, which proves them wired without
// writing anything.
func TestWritesAreRefusedWithoutTheClipboardOpen(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread() // a thread that has never opened the clipboard
		w := systemClipboard()
		if err := w.empty(); err == nil {
			t.Error("EmptyClipboard succeeded without the clipboard open")
		}
		h, err := newBlock([]byte("x\x00"))
		if err != nil {
			t.Error(err)
			return
		}
		if err := w.set(cfUnicodeText, h); err == nil {
			t.Error("SetClipboardData succeeded without the clipboard open")
			return // the clipboard owns the block now
		}
		_, _ = foundation.GlobalFree(foundation.HGLOBAL(h))
	}()
	<-done
}

// Every canonical format is held, under the format name applications share
// for it, and PDF under a name of weave's own, reported private.
func TestSupportNamesEachFormat(t *testing.T) {
	c, _ := backend(t)
	s := c.Support()
	if s.SingleRepresentation || len(s.Formats) != len(weavewire.ClipboardFormats()) {
		t.Fatalf("support %+v", s)
	}
	for _, f := range s.Formats {
		if !f.Held || f.Native == "" || f.Private != (f.Format == weavewire.ClipboardPDF) {
			t.Errorf("%+v", f)
		}
	}
	if pdf := s.Formats[3]; pdf.Format != weavewire.ClipboardPDF ||
		pdf.Native != "Portable Document Format" {
		t.Errorf("PDF %+v", pdf)
	}
}

// PDF copied by another application under the same registered name is read
// as PDF.
func TestReadsAPDFAnotherApplicationCopied(t *testing.T) {
	c, f := backend(t)
	f.put(t, c.formatID(weavewire.ClipboardPDF), []byte("%PDF-1.7 from another application"))
	got, err := c.Read(
		context.Background(),
		[]weavewire.ClipboardFormat{weavewire.ClipboardPDF},
		1<<20,
	)
	if err != nil || len(got.Items) != 1 ||
		string(got.Items[0].Data) != "%PDF-1.7 from another application" {
		t.Fatalf("read %+v, %v", got.Items, err)
	}
	if id := c.formatID(
		weavewire.ClipboardPDF,
	); f.regs["Portable Document Format"] != id ||
		id == 0 {
		t.Errorf("PDF registered as %d, %v", id, f.regs)
	}
}

// paintCopy is what Paint puts on the clipboard for a copy: a 24-bit CF_DIB,
// three pixels wide so its rows are padded, and the CF_BITMAP it came from,
// with no PNG.
func paintCopy() []byte {
	return dibFixture{
		header: 40, width: 3, height: 2, bitCount: 24,
		rows: [][]byte{{0, 0, 255, 0, 255, 0, 255, 0, 0}, {255, 255, 255, 0, 0, 0, 0, 0, 255}},
	}.build()
}

// A bitmap with no PNG beside it, as Paint copies, is offered and read as
// PNG, and a host that never reads it still learns its size from stat.
func TestABitmapOnlyCopyIsPNG(t *testing.T) {
	c, f := backend(t)
	f.put(t, cfBitmap, []byte("an HBITMAP"))
	f.put(t, cfDIB, paintCopy())

	st, err := c.Stat(context.Background())
	if err != nil ||
		!slices.Equal(
			st.Formats,
			[]weavewire.ClipboardFormatInfo{{Format: weavewire.ClipboardPNG}},
		) {
		t.Fatalf("stat %+v, %v; want PNG", st, err)
	}
	got, err := c.Read(
		context.Background(),
		[]weavewire.ClipboardFormat{weavewire.ClipboardPNG},
		1<<20,
	)
	if err != nil || len(got.Items) != 1 {
		t.Fatalf("read %+v, %v", got.Items, err)
	}
	samePixels(t, "Paint copy", pixels(t, got.Items[0].Data), [][]color.NRGBA{
		{rgb(255, 0, 0), rgb(0, 255, 0), rgb(0, 0, 255)},
		{rgb(255, 255, 255), rgb(0, 0, 0), rgb(255, 0, 0)},
	})

	h := weavemoduletest.Start(t, weaveclipboard.NewService(c))
	var sized weavewire.ClipboardStatResponse
	h.Decode(weavewire.KindClipboardStat, nil, &sized)
	if len(sized.Formats) != 1 || sized.Formats[0].Size != int64(len(got.Items[0].Data)) {
		t.Errorf(
			"stat through the service %+v, want PNG at %d bytes",
			sized.Formats,
			len(got.Items[0].Data),
		)
	}
}

// The bitmap an application wrote is listed before the ones Windows
// synthesizes from it, and is the one read; one that does not convert gives
// way to the next.
func TestTheBitmapReadIsTheFirstThatConverts(t *testing.T) {
	alpha := dibFixture{
		header: v5HeaderSize, width: 1, height: 1, bitCount: 32, compression: biBitfields,
		masks: []uint32{0xff0000, 0xff00, 0xff, 0xff000000}, rows: [][]byte{{30, 20, 10, 128}},
	}.build()
	c, f := backend(t)
	f.put(t, cfDIB, []byte("not a bitmap"))
	f.put(t, cfDIBV5, alpha)
	got, err := c.Read(context.Background(), nil, 1<<20)
	if err != nil || len(got.Items) != 1 {
		t.Fatalf("read %+v, %v", got.Items, err)
	}
	samePixels(
		t,
		"CF_DIBV5",
		pixels(t, got.Items[0].Data),
		[][]color.NRGBA{{rgba(10, 20, 30, 128)}},
	)

	// One that cannot be fetched, or read, is passed over too; with none
	// left, there is no image.
	f.failGet = cfDIBV5
	_, _ = foundation.GlobalFree(foundation.HGLOBAL(f.blocks[cfDIB]))
	f.blocks[cfDIB] = 0 // a block GlobalSize cannot size
	if got, err := c.Read(context.Background(), nil, 1<<20); err != nil || len(got.Items) != 0 {
		t.Errorf("read %+v, %v; want nothing", got.Items, err)
	}

	// CF_BITMAP alone is offered: Windows synthesizes CF_DIB from it for any
	// reader. The stand-in does not, so there is nothing to read here.
	c, f = backend(t)
	f.put(t, cfBitmap, []byte("an HBITMAP"))
	if o := c.availableOpen(t, f); o.png || !slices.Equal(o.bitmaps, []uint32{cfDIB}) ||
		!slices.Equal(o.formats, []weavewire.ClipboardFormat{weavewire.ClipboardPNG}) {
		t.Errorf("CF_BITMAP alone offers %+v, want CF_DIB to be asked for", o)
	}
}

// availableOpen is available with the stand-in clipboard open.
func (c *clipboard) availableOpen(t *testing.T, f *fakeWin) offer {
	t.Helper()
	f.open = true
	defer func() { f.open = false }()
	return c.available()
}

// A PNG from the host is written as PNG and as CF_DIBV5 and CF_DIB, which
// Paint pastes; the PNG is what reads back.
func TestAPNGIsWrittenAsBitmapsToo(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	img.SetNRGBA(0, 0, rgba(255, 0, 0, 255))
	img.SetNRGBA(1, 0, rgba(0, 255, 0, 128))
	img.SetNRGBA(2, 0, rgba(0, 0, 255, 0))
	data := encodePNG(t, img)

	c, f := backend(t)
	res, err := c.Write(
		context.Background(),
		[]weavewire.ClipboardItem{{Format: weavewire.ClipboardPNG, Data: data}},
	)
	if err != nil ||
		!slices.Equal(res.Written, []weavewire.ClipboardFormat{weavewire.ClipboardPNG}) {
		t.Fatalf("write %+v, %v", res, err)
	}
	v5, err := dibToPNG(f.held(t, cfDIBV5))
	if err != nil {
		t.Fatalf("CF_DIBV5: %v", err)
	}
	samePixels(t, "CF_DIBV5", pixels(t, v5), [][]color.NRGBA{
		{rgb(255, 0, 0), rgba(0, 255, 0, 128), rgba(0, 0, 255, 0)},
	})
	flat, err := dibToPNG(f.held(t, cfDIB))
	if err != nil {
		t.Fatalf("CF_DIB: %v", err)
	}
	samePixels(t, "CF_DIB", pixels(t, flat), [][]color.NRGBA{
		{rgb(255, 0, 0), rgb(127, 255, 127), rgb(255, 255, 255)},
	})
	got, err := c.Read(context.Background(), nil, 1<<20)
	if err != nil || len(got.Items) != 1 || !bytes.Equal(got.Items[0].Data, data) {
		t.Errorf("read back %+v, %v; want the PNG as sent", got.Items, err)
	}

	// No bitmap from bytes that are no PNG, and a bitmap the clipboard
	// refuses fails nothing: the PNG is on it either way.
	c, f = backend(t)
	if _, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardPNG, Data: []byte("\x89PNG, not really")},
	}); err != nil || f.held(t, cfDIB) != nil || f.held(t, cfDIBV5) != nil {
		t.Errorf("an undecodable PNG: %v, CF_DIB %v", err, f.held(t, cfDIB) != nil)
	}
	c, f = backend(t)
	f.failSet = cfDIBV5
	if res, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardPNG, Data: data},
	}); err != nil || len(res.Written) != 1 || f.held(t, cfDIB) == nil {
		t.Errorf("a refused CF_DIBV5: %+v, %v", res, err)
	}
}
