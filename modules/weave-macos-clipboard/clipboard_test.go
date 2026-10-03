//go:build darwin

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/appkit"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/foundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/obj"
	"github.com/ebitengine/purego/objc"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// private is the real backend over a pasteboard of the test's own: every
// NSPasteboard call is the real one, and the clipboard of the machine running
// the test is never written.
func private(t *testing.T) *clipboard {
	t.Helper()
	pb := appkit.PasteboardWithUniqueName()
	if pb == nil {
		t.Skip("no pasteboard server: not running in a GUI session")
	}
	t.Cleanup(pb.ReleaseGlobally)
	return &clipboard{board: func() *appkit.Pasteboard { return pb }, stage: &stager{}}
}

func TestWriteThenStatAndRead(t *testing.T) {
	c := private(t)
	ctx := context.Background()

	empty, err := c.Stat(ctx)
	if err != nil || len(empty.Formats) != 0 {
		t.Fatalf("a new pasteboard: %+v, %v", empty, err)
	}

	res, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("héllo")},
		{Format: weavewire.ClipboardHTML, Data: []byte("<b>héllo</b>")},
		{Format: weavewire.ClipboardPNG, Data: []byte("\x89PNG\r\n")},
		{Format: weavewire.ClipboardText, Data: []byte("a second text is not a second item")},
		{Format: "application/x-unknown", Data: []byte("?")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []weavewire.ClipboardFormat{
		weavewire.ClipboardText, weavewire.ClipboardHTML, weavewire.ClipboardPNG,
	}; !slices.Equal(res.Written, want) {
		t.Errorf("written %v, want %v", res.Written, want)
	}
	if res.ChangeToken == empty.ChangeToken {
		t.Error("the change count did not move")
	}

	st, err := c.Stat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ChangeToken != res.ChangeToken {
		t.Errorf("stat token %d, set token %d", st.ChangeToken, res.ChangeToken)
	}
	// The pasteboard also offers what it can translate the image to (TIFF);
	// what was written must be there, richest first.
	written := slices.DeleteFunc(
		slices.Clone(st.Formats),
		func(f weavewire.ClipboardFormatInfo) bool {
			return f.Format == weavewire.ClipboardTIFF
		},
	)
	if want := []weavewire.ClipboardFormatInfo{
		{
			Format: weavewire.ClipboardPNG,
		},
		{Format: weavewire.ClipboardHTML},
		{Format: weavewire.ClipboardText},
	}; !slices.Equal(written, want) {
		t.Errorf("formats %+v, want %+v", st.Formats, want)
	}

	all, err := c.Read(ctx, []weavewire.ClipboardFormat{
		weavewire.ClipboardPNG, weavewire.ClipboardHTML, weavewire.ClipboardText,
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(all.Items))
	for _, it := range all.Items {
		got = append(got, string(it.Format)+"="+string(it.Data))
	}
	if want := []string{
		"image/png=\x89PNG\r\n", "text/html=<b>héllo</b>", "text/plain=héllo",
	}; !slices.Equal(got, want) || all.ChangeToken != st.ChangeToken {
		t.Errorf("read %q at %d, want %q at %d", got, all.ChangeToken, want, st.ChangeToken)
	}

	text, err := c.Read(ctx, []weavewire.ClipboardFormat{weavewire.ClipboardText}, 1<<20)
	if err != nil || len(text.Items) != 1 || string(text.Items[0].Data) != "héllo" {
		t.Errorf("text only: %+v, %v", text.Items, err)
	}
}

func TestFilesRoundTripAsRealFiles(t *testing.T) {
	c := private(t)
	ctx := context.Background()
	res, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "a.txt", Data: []byte("first")},
		{
			Format: weavewire.ClipboardFiles,
			Name:   "/elsewhere/a.txt",
			Data:   []byte("second, larger"),
		},
		{Format: weavewire.ClipboardText, Data: []byte("a.txt")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []weavewire.ClipboardFormat{
		weavewire.ClipboardFiles,
		weavewire.ClipboardText,
	}; !slices.Equal(
		res.Written,
		want,
	) {
		t.Errorf("written %v, want %v", res.Written, want)
	}

	st, err := c.Stat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(st.Formats, weavewire.ClipboardFormatInfo{
		Format: weavewire.ClipboardFiles, Size: 19, Count: 2,
	}) {
		t.Errorf("formats %+v, want two files of 19 bytes", st.Formats)
	}

	got, err := c.Read(ctx, []weavewire.ClipboardFormat{weavewire.ClipboardFiles}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || string(got.Items[0].Data) != "first" ||
		got.Items[0].Name != "a.txt" ||
		got.Items[1].Data != nil ||
		got.Items[1].Size != 14 {
		t.Errorf("read %+v: want the first file and the second sized but over the cap", got.Items)
	}
}

func TestStatLeavesOutFilesThatAreGone(t *testing.T) {
	c := private(t)
	ctx := context.Background()
	if _, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "gone.txt", Data: []byte("x")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(c.stage.root); err != nil {
		t.Fatal(err)
	}
	st, err := c.Stat(ctx)
	if err != nil || len(st.Formats) != 0 {
		t.Errorf("stat = %+v, %v; a file that is gone is nothing to copy", st, err)
	}
}

func TestWriteRefusesABadFileName(t *testing.T) {
	c := private(t)
	if _, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "..", Data: []byte("x")},
	}); err == nil {
		t.Fatal("staged a file named ..")
	}
}

// Finder copies files as file reference URLs, which name an inode; they must
// resolve to the file's path.
func TestFileURLPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	u := foundation.NewURLFileURLWithPath(p)
	refID := objc.Send[objc.ID](obj.ID(u), objc.RegisterName("fileReferenceURL"))
	ref := foundation.URLFromID(refID).AbsoluteString()
	if got, ok := fileURLPath(ref); !ok || got != real {
		t.Errorf("reference URL %q resolved to %q, want %q", ref, got, real)
	}
	for in, want := range map[string]string{
		"file:///Users/me/a%20b.txt": "/Users/me/a b.txt",
		"https://example.com/":       "",
		"file://":                    "",
		"%zz":                        "",
		"file:///.file/id=1.1/":      "", // an inode that does not exist
	} {
		got, ok := fileURLPath(in)
		if got != want || ok != (want != "") {
			t.Errorf("fileURLPath(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}

// Every call drains its own pool on a locked thread; concurrent ops must
// neither race nor leave one call's thread state to another.
func TestConcurrentOps(t *testing.T) {
	c := private(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if i%2 == 0 {
				_, _ = c.Write(ctx, []weavewire.ClipboardItem{
					{Format: weavewire.ClipboardText, Data: []byte{byte('a' + i)}},
				})
				return
			}
			_, _ = c.Read(ctx, nil, 1<<20)
			_, _ = c.Stat(ctx)
		})
	}
	wg.Wait()
	if st, err := c.Stat(ctx); err != nil || len(st.Formats) != 1 {
		t.Errorf("after concurrent writes: %+v, %v", st, err)
	}
}

// The real general pasteboard, read only: the backend reaches the console
// user's pasteboard server. Nothing is written and nothing read is logged.
func TestStatOfTheGeneralPasteboard(t *testing.T) {
	c := newClipboard()
	if appkit.GeneralPasteboard() == nil {
		t.Skip("no pasteboard server: not running in a GUI session")
	}
	if _, err := c.Stat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(
		context.Background(),
		[]weavewire.ClipboardFormat{weavewire.ClipboardText},
		1<<10,
	); err != nil {
		t.Fatal(err)
	}
}
