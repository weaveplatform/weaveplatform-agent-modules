//go:build windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"a.txt": "a.txt", "dir/b.txt": "b.txt", `C:\x\c.txt`: "c.txt", "../../etc/passwd": "passwd", "report.txt:hidden": "", "a?.txt": "",
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
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readFiles([]string{ok, dir, filepath.Join(dir, "gone")}, 10)
	if len(got) != 1 || got[0].Name != "ok" || string(got[0].Data) != "1" {
		t.Errorf("read %+v, want ok alone", got)
	}
}

func TestStagerReportsAStagingFailure(t *testing.T) {
	item := []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "a", Data: []byte("x")},
	}

	absent := filepath.Join(t.TempDir(), "absent")
	t.Setenv("TMP", absent)
	t.Setenv("TEMP", absent)
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
}

func TestDropFilesLayout(t *testing.T) {
	got := dropFiles([]string{"C:\\a"})
	want := []byte{
		20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0,
		'C', 0, ':', 0, '\\', 0, 'a', 0, 0, 0, 0, 0,
	}
	if !slices.Equal(got, want) {
		t.Errorf("DROPFILES % x, want % x", got, want)
	}
}
