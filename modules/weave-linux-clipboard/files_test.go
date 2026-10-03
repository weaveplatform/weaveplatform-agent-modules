//go:build linux

package main

import (
	"os"
	"path/filepath"
	"slices"
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

func TestRichest(t *testing.T) {
	items := func(fs ...weavewire.ClipboardFormat) []weavewire.ClipboardItem {
		out := make([]weavewire.ClipboardItem, 0, len(fs))
		for _, f := range fs {
			out = append(out, weavewire.ClipboardItem{Format: f})
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
		got, ok := richest(tc.in)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: %q %v, want %q", name, got, ok, tc.want)
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

func TestReadFilesSkipsWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	ok, locked := filepath.Join(dir, "ok"), filepath.Join(dir, "locked")
	if err := os.WriteFile(ok, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(locked, []byte("2"), 0o000); err != nil {
		t.Fatal(err)
	}
	got := readFiles([]string{ok, locked, dir, filepath.Join(dir, "gone")}, 10)
	names := make([]string, 0, len(got))
	for _, it := range got {
		names = append(names, it.Name)
	}
	want := []string{"ok"}
	if os.Geteuid() == 0 {
		want = []string{"ok", "locked"} // root reads a mode-000 file
	}
	if !slices.Equal(names, want) {
		t.Errorf("read %q, want %q", names, want)
	}
	if baseName("plain") != "plain" {
		t.Error("baseName of a bare name")
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
