//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

func TestDetectPicksTheSessionsTool(t *testing.T) {
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	only := func(names ...string) func(string) bool {
		return func(n string) bool { return slices.Contains(names, n) }
	}
	for name, tc := range map[string]struct {
		wayland, display string
		look             func(string) bool
		cmd, missing     string
	}{
		"wayland":                   {"wayland-0", "", all, "wl-paste", ""},
		"wayland over xwayland":     {"wayland-0", ":0", all, "wl-paste", ""},
		"wayland without wl-copy":   {"wayland-0", "", only("wl-paste"), "", "wl-clipboard"},
		"wayland falls back to x11": {"wayland-0", ":0", only("xclip"), "xclip", ""},
		"x11":                       {"", ":0", all, "xclip", ""},
		"x11 without xclip":         {"", ":1", none, "", "xclip"},
		"no display":                {"", "", all, "", "no Wayland or X11 display"},
	} {
		env := map[string]string{"WAYLAND_DISPLAY": tc.wayland, "DISPLAY": tc.display}
		c := detect(func(k string) string { return env[k] }, tc.look)
		switch {
		case tc.cmd != "" && (c.tool == nil || c.tool.cmd != tc.cmd):
			t.Errorf("%s: tool %+v (%s), want %s", name, c.tool, c.missing, tc.cmd)
		case tc.cmd == "" && (c.tool != nil || !strings.Contains(c.missing, tc.missing)):
			t.Errorf(
				"%s: tool %+v, missing %q, want one naming %q",
				name,
				c.tool,
				c.missing,
				tc.missing,
			)
		}
	}
}

// With no tool every op is an unsupported answer naming what is missing, so
// a host can tell a session it cannot sync from a broken module.
func TestWithoutAToolEveryOpIsUnsupported(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")
	c := newClipboard()
	ctx := context.Background()
	_, statErr := c.Stat(ctx)
	_, readErr := c.Read(ctx, nil, 1<<20)
	_, writeErr := c.Write(ctx, []weavewire.ClipboardItem{{Format: weavewire.ClipboardText}})
	for kind, err := range map[string]error{
		weavewire.KindClipboardStat: statErr,
		weavewire.KindClipboardGet:  readErr,
		weavewire.KindClipboardSet:  writeErr,
	} {
		u, ok := errors.AsType[*weavewire.UnsupportedError](err)
		if !ok || u.Kind != kind || !strings.Contains(u.Reason, "DISPLAY") {
			t.Errorf("%s: err = %v, want unsupported naming the missing display", kind, err)
		}
	}
}

func TestStatReportsFormatsSizesAndAContentToken(t *testing.T) {
	for name, build := range map[string]func(*testing.T) *clipboard{"wayland": wayland, "x11": x11} {
		t.Run(name, func(t *testing.T) {
			f := installFake(t)
			c := build(t)
			ctx := context.Background()

			empty, err := c.Stat(ctx)
			if err != nil || len(empty.Formats) != 0 {
				t.Fatalf("empty clipboard: %+v, %v", empty, err)
			}

			file := filepath.Join(t.TempDir(), "notes.txt")
			if err := os.WriteFile(file, []byte("four"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.offer(t, []string{"TARGETS", "SAVE_TARGETS"}, map[string]string{
				"UTF8_STRING":   "hello",
				"text/html":     "<b>hello</b>",
				"image/png":     "\x89PNG",
				"text/uri-list": "file://" + file + "\r\nfile:///nonexistent\r\n",
			})
			st, err := c.Stat(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := []weavewire.ClipboardFormatInfo{
				{Format: weavewire.ClipboardFiles, Size: 4, Count: 1},
				{Format: weavewire.ClipboardPNG, Size: 4},
				{Format: weavewire.ClipboardHTML, Size: 12},
				{Format: weavewire.ClipboardText, Size: 5},
			}
			if !slices.Equal(st.Formats, want) {
				t.Errorf("formats = %+v, want %+v", st.Formats, want)
			}
			again, _ := c.Stat(ctx)
			if again.ChangeToken != st.ChangeToken || st.ChangeToken == empty.ChangeToken {
				t.Errorf(
					"tokens: empty %d, first %d, unchanged %d",
					empty.ChangeToken,
					st.ChangeToken,
					again.ChangeToken,
				)
			}

			// Same targets, new image: only a digest of the content sees it.
			f.offer(t, []string{"TARGETS", "SAVE_TARGETS"}, map[string]string{
				"UTF8_STRING":   "hello",
				"text/html":     "<b>hello</b>",
				"image/png":     "\x89PNG2",
				"text/uri-list": "file://" + file + "\r\n",
			})
			if changed, _ := c.Stat(ctx); changed.ChangeToken == st.ChangeToken {
				t.Error("a new image over the old one left the token unchanged")
			}
		})
	}
}

func TestStatLeavesOutAFileListWithNothingToCopy(t *testing.T) {
	f := installFake(t)
	c := wayland(t)
	f.offer(t, nil, map[string]string{"text/uri-list": "file://" + t.TempDir() + "\r\n"})
	st, err := c.Stat(context.Background())
	if err != nil || len(st.Formats) != 0 {
		t.Fatalf("stat = %+v, %v; a directory is not a copied file", st, err)
	}
}

func TestReadReturnsWhatWasAskedFor(t *testing.T) {
	f := installFake(t)
	c := wayland(t)
	ctx := context.Background()
	dir := t.TempDir()
	small, big := filepath.Join(dir, "small.txt"), filepath.Join(dir, "big.bin")
	if err := os.WriteFile(small, []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	f.offer(t, nil, map[string]string{
		"text/plain;charset=utf-8": "small.txt big.bin",
		"application/rtf":          `{\rtf1}`,
		"text/uri-list":            "# copied\r\nfile://" + small + "\r\nfile://" + big + "\r\nfile://" + dir + "\r\n",
	})

	all, err := c.Read(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := c.Stat(ctx)
	if all.ChangeToken != st.ChangeToken {
		t.Errorf("read token %d, stat token %d", all.ChangeToken, st.ChangeToken)
	}
	got := make([]string, 0, len(all.Items))
	for _, it := range all.Items {
		got = append(got, string(it.Format)+":"+it.Name+":"+string(it.Data))
	}
	// Every representation asked for, the file list's name text included:
	// which of them to sync is the host's call. The big file comes back
	// sized and empty, for the service to list as omitted.
	want := []string{
		"files:small.txt:ab",
		"files:big.bin:",
		`text/rtf::{\rtf1}`,
		"text/plain::small.txt big.bin",
	}
	if !slices.Equal(got, want) {
		t.Errorf("items = %q, want %q", got, want)
	}
	if all.Items[1].Size != 100 {
		t.Errorf("omitted file size = %d, want 100", all.Items[1].Size)
	}

	text, err := c.Read(ctx, []weavewire.ClipboardFormat{weavewire.ClipboardText}, 1<<20)
	if err != nil || len(text.Items) != 1 || text.Items[0].Format != weavewire.ClipboardText {
		t.Errorf("text only: %+v, %v", text.Items, err)
	}
}

func TestReadSkipsARepresentationThatVanished(t *testing.T) {
	f := installFake(t)
	c := x11(t)
	// Offered but unreadable: the owner changed between the list and the
	// read.
	f.write(t, "targets", "TARGETS\nimage/png\nUTF8_STRING\n")
	if err := os.MkdirAll(filepath.Join(f.dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.write(t, "data/UTF8_STRING", "still here")
	got, err := c.Read(context.Background(), nil, 1<<20)
	if err != nil || len(got.Items) != 1 || string(got.Items[0].Data) != "still here" {
		t.Fatalf("read = %+v, %v", got.Items, err)
	}
}

func TestWriteHoldsTheRichestRepresentation(t *testing.T) {
	for name, tc := range map[string]struct {
		build  func(*testing.T) *clipboard
		text   string
		items  []weavewire.ClipboardItem
		target string
		want   weavewire.ClipboardFormat
	}{
		"wayland text": {
			build: wayland, target: "text/plain;charset=utf-8", want: weavewire.ClipboardText,
			items: []weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Data: []byte("hi")}},
		},
		"x11 text": {
			build: x11, target: "UTF8_STRING", want: weavewire.ClipboardText,
			items: []weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Data: []byte("hi")}},
		},
		"html over text": {
			build: wayland, target: "text/html", want: weavewire.ClipboardHTML,
			items: []weavewire.ClipboardItem{
				{Format: weavewire.ClipboardText, Data: []byte("hi")},
				{Format: weavewire.ClipboardHTML, Data: []byte("<i>hi</i>")},
				{Format: "application/x-unknown", Data: []byte("?")},
			},
		},
		"image over html": {
			build: x11, target: "image/png", want: weavewire.ClipboardPNG,
			items: []weavewire.ClipboardItem{
				{Format: weavewire.ClipboardHTML, Data: []byte("<img>")},
				{Format: weavewire.ClipboardPNG, Data: []byte("\x89PNG")},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := installFake(t)
			c := tc.build(t)
			res, err := c.Write(context.Background(), tc.items)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.Written, []weavewire.ClipboardFormat{tc.want}) {
				t.Errorf("written = %v, want %s", res.Written, tc.want)
			}
			var want string
			for _, it := range tc.items {
				if it.Format == tc.want {
					want = string(it.Data)
				}
			}
			if got, ok := f.held(t, tc.target); !ok || got != want {
				t.Errorf("clipboard holds %q under %s, want %q", got, tc.target, want)
			}
			st, _ := c.Stat(context.Background())
			if res.ChangeToken != st.ChangeToken {
				t.Errorf("set token %d, stat afterwards %d", res.ChangeToken, st.ChangeToken)
			}
		})
	}
}

func TestWriteStagesFilesAndOffersTheirPaths(t *testing.T) {
	f := installFake(t)
	c := wayland(t)
	ctx := context.Background()
	items := []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("a.txt")},
		{Format: weavewire.ClipboardFiles, Name: "a.txt", Data: []byte("first")},
		{Format: weavewire.ClipboardFiles, Name: `C:\Users\me\a.txt`, Data: []byte("second")},
	}
	res, err := c.Write(ctx, items)
	if err != nil ||
		!slices.Equal(res.Written, []weavewire.ClipboardFormat{weavewire.ClipboardFiles}) {
		t.Fatalf("write = %+v, %v", res, err)
	}
	list, _ := f.held(t, "text/uri-list")
	paths := parseURIList([]byte(list))
	if len(paths) != 2 || filepath.Base(paths[0]) != "a.txt" || filepath.Base(paths[1]) != "a.txt" {
		t.Fatalf("offered %q", list)
	}
	for i, want := range []string{"first", "second"} {
		if b, err := os.ReadFile(paths[i]); err != nil || string(b) != want {
			t.Errorf("%s holds %q, %v; want %q", paths[i], b, err, want)
		}
	}
	// The files read back as the host sent them.
	got, err := c.Read(ctx, []weavewire.ClipboardFormat{weavewire.ClipboardFiles}, 1<<20)
	if err != nil || len(got.Items) != 2 || string(got.Items[1].Data) != "second" {
		t.Errorf("read back %+v, %v", got.Items, err)
	}

	// The next copy replaces the staged files.
	if _, err := c.Write(ctx, items[1:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[1]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an earlier copy's file survived: %v", err)
	}
}

func TestWriteOfNothingItCanHoldChangesNothing(t *testing.T) {
	f := installFake(t)
	c := wayland(t)
	f.offer(t, nil, map[string]string{"text/html": "<p>kept</p>"})
	before, _ := c.Stat(context.Background())
	res, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: "application/x-unknown", Data: []byte("?")},
	})
	if err != nil || len(res.Written) != 0 || res.ChangeToken != before.ChangeToken {
		t.Fatalf("write = %+v, %v; want nothing written and the token unchanged", res, err)
	}
	if got, _ := f.held(t, "text/html"); got != "<p>kept</p>" {
		t.Errorf("clipboard now %q", got)
	}
}

// A clipboard that cannot be read is empty to the module rather than broken:
// both tools exit non-zero for an empty clipboard, which is the common case.
func TestAFailingToolReadsAsEmpty(t *testing.T) {
	f := installFake(t)
	c := wayland(t)
	f.offer(t, nil, map[string]string{"UTF8_STRING": "x"})
	f.fail(t)
	st, err := c.Stat(context.Background())
	if err != nil || len(st.Formats) != 0 {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	if _, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("x")},
	}); err == nil {
		t.Error("a failed copy reported success")
	}
}

// A tool that cannot be run at all is a failure, not an empty clipboard.
func TestAToolThatCannotRunIsAnError(t *testing.T) {
	f := installFake(t)
	c := wayland(t)
	f.offer(t, nil, map[string]string{"UTF8_STRING": "x"})
	c.tool.cmd = filepath.Join(t.TempDir(), "absent")
	ctx := context.Background()
	if _, err := c.Stat(ctx); err == nil {
		t.Error("stat succeeded")
	}
	if _, err := c.Read(ctx, nil, 1); err == nil {
		t.Error("read succeeded")
	}
	// The copy itself runs, but the token afterwards cannot be read.
	if _, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("x")},
	}); err == nil {
		t.Error("write succeeded")
	}
	if _, err := c.Write(ctx, []weavewire.ClipboardItem{{Format: "x/unknown"}}); err == nil {
		t.Error("write of nothing succeeded")
	}
}

func TestWriteRefusesAFileWithNoName(t *testing.T) {
	installFake(t)
	c := wayland(t)
	_, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "..", Data: []byte("x")},
	})
	if !errors.Is(err, errBadName) {
		t.Fatalf("err = %v, want errBadName", err)
	}
}

// Every set moves the token, even one that writes what the clipboard held.
func TestEverySetChangesTheToken(t *testing.T) {
	installFake(t)
	c := wayland(t)
	items := []weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Data: []byte("same")}}
	first, err := c.Write(context.Background(), items)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Write(context.Background(), items)
	if err != nil || second.ChangeToken == first.ChangeToken {
		t.Fatalf("tokens %d then %d, %v", first.ChangeToken, second.ChangeToken, err)
	}
}

func TestSupportIsOneRepresentationPerSet(t *testing.T) {
	installFake(t)
	for _, c := range []*clipboard{wayland(t), detect(func(string) string { return "" }, lookPath)} {
		s := c.Support()
		if !s.SingleRepresentation || s.Limitation == "" ||
			len(s.Formats) != len(weavewire.ClipboardFormats()) {
			t.Fatalf("support %+v", s)
		}
		for _, f := range s.Formats {
			if !f.Held || f.Native == "" {
				t.Errorf("%+v", f)
			}
		}
	}
}
