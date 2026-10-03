//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
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

// swap replaces *v for the test and restores it afterwards.
func swap[T any](t *testing.T, v *T, with T) {
	t.Helper()
	old := *v
	*v = with
	t.Cleanup(func() { *v = old })
}

var errInjected = errors.New("injected")

func TestStagerStagesOnlyFiles(t *testing.T) {
	s := &stager{root: t.TempDir()}
	paths, err := s.files([]weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("not a file")},
		{Format: weavewire.ClipboardFiles, Name: "a", Data: []byte("x")},
	})
	if err != nil || len(paths) != 1 || filepath.Base(paths[0]) != "a" {
		t.Errorf("staged %v, %v; want the one file", paths, err)
	}
}

func TestStagerReportsEachFilesystemFailure(t *testing.T) {
	item := []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "a", Data: []byte("x")},
	}
	fail2 := func(string, os.FileMode) error { return errInjected }
	for name, inject := range map[string]func(t *testing.T){
		"clear": func(t *testing.T) { swap(t, &removeAll, func(string) error { return errInjected }) },
		"mkdir": func(t *testing.T) { swap(t, &mkdir, fail2) },
		"write": func(t *testing.T) {
			swap(t, &writeFile, func(string, []byte, os.FileMode) error { return errInjected })
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := &stager{root: t.TempDir()}
			// A previous copy leaves something for the clear to remove.
			if _, err := s.files(item); err != nil {
				t.Fatal(err)
			}
			inject(t)
			if _, err := s.files(item); !errors.Is(err, errInjected) {
				t.Errorf("staged with %s failing: %v", name, err)
			}
		})
	}
}

func TestReadFilesSkipsAFileItCannotRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	swap(t, &readFile, func(string) ([]byte, error) { return nil, errInjected })
	if got := readFiles([]string{p}, 10); len(got) != 0 {
		t.Errorf("read %+v from a failing read", got)
	}
}
