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

// Captured from Nautilus, Dolphin and Firefox copies.
func TestParseURIList(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want []string
	}{
		"nautilus": {
			"file:///home/me/Documents/report.pdf\r\nfile:///home/me/Pictures/a%20b.png\r\n",
			[]string{"/home/me/Documents/report.pdf", "/home/me/Pictures/a b.png"},
		},
		"dolphin, LF and a trailing line": {
			"file:///home/me/x.txt\nfile:///home/me/y.txt", []string{"/home/me/x.txt", "/home/me/y.txt"},
		},
		"comments and blanks": {"# a comment\r\n\r\nfile:///tmp/z\r\n", []string{"/tmp/z"}},
		"localhost":           {"file://localhost/etc/hosts\r\n", []string{"/etc/hosts"}},
		"a browser link":      {"https://example.com/page\r\n", nil},
		"another host":        {"file://server/share/f\r\n", nil},
		"no path":             {"file://\r\n", nil},
		"not a uri":           {"%zz\r\n", nil},
		"empty":               {"", nil},
	} {
		if got := parseURIList([]byte(tc.in)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestURIListRoundTrips(t *testing.T) {
	paths := []string{"/tmp/weave-clipboard-1/0/a b.txt", "/tmp/weave-clipboard-1/1/ü#.txt"}
	got := uriList(paths)
	if string(
		got,
	) != "file:///tmp/weave-clipboard-1/0/a%20b.txt\r\nfile:///tmp/weave-clipboard-1/1/%C3%BC%23.txt\r\n" {
		t.Errorf("uri list %q", got)
	}
	if back := parseURIList(got); !slices.Equal(back, paths) {
		t.Errorf("parsed back %q", back)
	}
}

// Targets as the X11 TARGETS atom list and wl-paste --list-types report them.
func TestPickTarget(t *testing.T) {
	x11 := []string{
		"TIMESTAMP",
		"TARGETS",
		"MULTIPLE",
		"SAVE_TARGETS",
		"UTF8_STRING",
		"STRING",
		"text/html",
	}
	gtk := []string{
		"text/plain; charset=utf-8",
		"text/plain",
		"TEXT",
		"image/png",
		"application/rtf",
	}
	for name, tc := range map[string]struct {
		offered []string
		format  weavewire.ClipboardFormat
		want    string
	}{
		"x11 text":         {x11, weavewire.ClipboardText, "UTF8_STRING"},
		"x11 html":         {x11, weavewire.ClipboardHTML, "text/html"},
		"gtk text":         {gtk, weavewire.ClipboardText, "text/plain; charset=utf-8"},
		"gtk rtf alias":    {gtk, weavewire.ClipboardRTF, "application/rtf"},
		"gtk png":          {gtk, weavewire.ClipboardPNG, "image/png"},
		"latin-1 only":     {[]string{"STRING"}, weavewire.ClipboardText, "STRING"},
		"case insensitive": {[]string{"Image/PNG"}, weavewire.ClipboardPNG, "Image/PNG"},
		"absent":           {x11, weavewire.ClipboardPDF, ""},
		"unknown format":   {x11, "application/x-other", ""},
	} {
		got, ok := pickTarget(tc.offered, tc.format)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: %q %v, want %q", name, got, ok, tc.want)
		}
	}
}

func TestSplitLines(t *testing.T) {
	if got := splitLines([]byte("TARGETS\r\n\n  text/html \nUTF8_STRING")); !slices.Equal(
		got, []string{"TARGETS", "text/html", "UTF8_STRING"},
	) {
		t.Errorf("lines %q", got)
	}
}

// A single-representation clipboard keeps the richest canonical format of a
// set; a set of none is unsupported.
func TestSingleKeepsTheRichest(t *testing.T) {
	items := func(fs ...weavewire.ClipboardFormat) []weavewire.ClipboardItem {
		out := make([]weavewire.ClipboardItem, 0, len(fs))
		for _, f := range fs {
			out = append(out, weavewire.ClipboardItem{Format: f, Name: "f"})
		}
		return out
	}
	for name, tc := range map[string]struct {
		in   []weavewire.ClipboardItem
		want weavewire.ClipboardFormat
	}{
		"files first":  {items(weavewire.ClipboardText, weavewire.ClipboardFiles, weavewire.ClipboardPNG), weavewire.ClipboardFiles},
		"pdf over rtf": {items(weavewire.ClipboardRTF, weavewire.ClipboardPDF), weavewire.ClipboardPDF},
		"tiff":         {items(weavewire.ClipboardTIFF, weavewire.ClipboardHTML), weavewire.ClipboardTIFF},
		"none":         {items("x/unknown"), ""},
	} {
		m := &memMech{label: "wl-copy", one: true}
		res, err := withMech(m).Write(context.Background(), tc.in)
		if tc.want == "" {
			if _, ok := errors.AsType[*weavewire.UnsupportedError](err); !ok {
				t.Errorf("%s: err = %v, want unsupported", name, err)
			}
			continue
		}
		if err != nil || !slices.Equal(res.Written, []weavewire.ClipboardFormat{tc.want}) ||
			len(m.offers) != 1 {
			t.Errorf(
				"%s: %+v, %v, offers %d; want %s alone",
				name,
				res,
				err,
				len(m.offers),
				tc.want,
			)
		}
	}
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"a.txt": "a.txt", "dir/b.txt": "b.txt", `C:\x\c.txt`: "c.txt", "../../etc/passwd": "passwd",
		"": "", ".": "", "..": "", "/": "", "a/..": "",
	} {
		got, err := fileName(in)
		if got != want || (err != nil) != (want == "") {
			t.Errorf("fileName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestStagerReportsAStagingFailure(t *testing.T) {
	item := []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "a", Data: []byte("x")},
	}

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))
	if _, err := (&stager{}).files(item); err == nil {
		t.Error("staged with no temporary directory")
	}

	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&stager{root: notDir}).files(item); err == nil {
		t.Error("staged into a file")
	}

	if os.Geteuid() != 0 {
		ro := t.TempDir()
		if err := os.Chmod(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
		if _, err := (&stager{root: ro}).files(item); err == nil {
			t.Error("staged into a read-only directory")
		}
	}
}

// The two staging steps that fail only on a filesystem's say-so: clearing a
// copy whose files cannot be removed, and writing a file the filesystem
// refuses the name of.
func TestStagerReportsEachFilesystemRefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes and writes what these refusals rely on")
	}
	root := t.TempDir()
	stuck := filepath.Join(root, "0", "sub")
	if err := os.MkdirAll(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })
	item := []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "a", Data: []byte("x")},
	}
	if _, err := (&stager{root: root}).files(item); err == nil ||
		!strings.Contains(err.Error(), "clearing") {
		t.Errorf("clearing a stuck copy: %v", err)
	}

	long := []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: strings.Repeat("n", 300)},
	}
	if _, err := (&stager{root: t.TempDir()}).files(long); err == nil {
		t.Error("staged a file whose name is too long")
	}
}
