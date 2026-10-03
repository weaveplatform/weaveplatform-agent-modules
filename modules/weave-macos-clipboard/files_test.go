//go:build darwin

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
