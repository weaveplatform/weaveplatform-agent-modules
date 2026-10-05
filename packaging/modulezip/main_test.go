package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const presenceManifest = `{"schema":1,"id":"weave-windows-presence","version":"0.2.0","protocol":1,
"zone":"A","privilege":"service","session":"system",
"platforms":[{"os":"windows","arch":"amd64"},{"os":"windows","arch":"arm64"}]}`

// peBinary stands in for a built module: what matters is the PE "MZ".
var peBinary = []byte("MZ\x90\x00 a module binary")

// fixture writes a module binary and manifest and returns their paths.
func fixture(t *testing.T, manifest string, binary []byte) (bin, man string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "weave-windows-presence-windows-amd64.exe")
	man = filepath.Join(dir, "module.manifest.json")
	if err := os.WriteFile(bin, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(man, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin, man
}

// unzip returns a zip's files in order, by name.
func unzip(t *testing.T, path string) ([]string, map[string]string) {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	files := map[string]string{}
	for _, f := range r.File {
		names = append(names, f.Name)
		if !f.Modified.Equal(modTime) {
			t.Errorf("%s modified %v, want %v", f.Name, f.Modified, modTime)
		}
		if f.Method != zip.Deflate {
			t.Errorf("%s stored with method %d, want deflate", f.Name, f.Method)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name] = string(b)
	}
	return names, files
}

func TestBuildPackagesTheModuleAndScripts(t *testing.T) {
	bin, man := fixture(t, presenceManifest, peBinary)
	out := filepath.Join(t.TempDir(), "dist")
	path, err := build(options{binary: bin, manifest: man, arch: "arm64", out: out})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "weave-windows-presence_0.2.0_windows_arm64.zip" {
		t.Fatalf("package file %s", filepath.Base(path))
	}
	names, files := unzip(t, path)
	want := []string{
		"install.ps1", "uninstall.ps1",
		"module/weave-windows-presence.exe", "module/module.manifest.json",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("zip holds %q, want %q", names, want)
	}
	for name, data := range map[string]string{
		"install.ps1":                       string(installScript),
		"uninstall.ps1":                     string(uninstallScript),
		"module/weave-windows-presence.exe": string(peBinary),
		"module/module.manifest.json":       presenceManifest,
	} {
		if files[name] != data {
			t.Errorf("%s holds %q", name, files[name])
		}
	}
}

// The same inputs make the same bytes, so a rebuilt package is checkably the
// one released.
func TestBuildIsReproducible(t *testing.T) {
	bin, man := fixture(t, presenceManifest, peBinary)
	var got [2][]byte
	for i := range got {
		path, err := build(options{binary: bin, manifest: man, arch: "amd64", out: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if got[i], err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got[0], got[1]) {
		t.Error("two builds of the same module differ")
	}
}

// -scripts packages the release's signed copies in place of the built-in
// scripts, byte for byte.
func TestBuildTakesScriptsFromADirectory(t *testing.T) {
	bin, man := fixture(t, presenceManifest, peBinary)
	dir := t.TempDir()
	for _, name := range scriptNames {
		data := "# " + name + "\n# SIG # Begin signature block\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path, err := build(options{binary: bin, manifest: man, arch: "amd64", out: t.TempDir(), scripts: dir})
	if err != nil {
		t.Fatal(err)
	}
	_, files := unzip(t, path)
	for _, name := range scriptNames {
		if want := "# " + name + "\n# SIG # Begin signature block\n"; files[name] != want {
			t.Errorf("%s holds %q, want %q", name, files[name], want)
		}
	}
}

func TestBuildRefuses(t *testing.T) {
	bin, man := fixture(t, presenceManifest, peBinary)
	badManifest := func(s string) string {
		_, m := fixture(t, s, peBinary)
		return m
	}
	elf, _ := fixture(t, presenceManifest, []byte("\x7fELF a linux binary"))
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		o    options
		want error
		text string
	}{
		{"arch", options{binary: bin, manifest: man, arch: "386"}, errArch, `"386"`},
		{"missing manifest", options{binary: bin, manifest: filepath.Join(t.TempDir(), "none"), arch: "amd64"},
			os.ErrNotExist, "read manifest"},
		{"manifest json", options{binary: bin, manifest: badManifest("{"), arch: "amd64"}, errManifest, ""},
		{"manifest id", options{binary: bin, manifest: badManifest(`{"id":"../x","version":"0.2.0"}`), arch: "amd64"},
			errManifest, "invalid module id"},
		{"manifest version", options{binary: bin, manifest: badManifest(`{"id":"x","version":"2"}`), arch: "amd64"},
			errManifest, "invalid version"},
		{"undeclared", options{binary: bin, manifest: badManifest(
			`{"id":"x","version":"0.2.0","platforms":[{"os":"linux","arch":"amd64"}]}`), arch: "amd64"},
			errNotDeclared, "windows/amd64"},
		{"missing binary", options{binary: filepath.Join(t.TempDir(), "none"), manifest: man, arch: "amd64"},
			os.ErrNotExist, "read binary"},
		{"not PE", options{binary: elf, manifest: man, arch: "amd64"}, errNotPE, ""},
		{"scripts", options{binary: bin, manifest: man, arch: "amd64", scripts: t.TempDir()},
			os.ErrNotExist, "read scripts"},
		{"out is a file", options{binary: bin, manifest: man, arch: "amd64", out: filepath.Join(file, "dist")},
			nil, "create output directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.o.out == "" {
				tc.o.out = t.TempDir()
			}
			_, err := build(tc.o)
			if err == nil {
				t.Fatal("build succeeded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.text) {
				t.Errorf("error %q does not mention %q", err, tc.text)
			}
		})
	}
}

// A directory standing where the package goes fails the write.
func TestBuildReportsAFailedWrite(t *testing.T) {
	bin, man := fixture(t, presenceManifest, peBinary)
	out := t.TempDir()
	if err := os.Mkdir(filepath.Join(out, "weave-windows-presence_0.2.0_windows_amd64.zip"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := build(options{binary: bin, manifest: man, arch: "amd64", out: out}); err == nil ||
		!strings.Contains(err.Error(), "write package") {
		t.Fatalf("error %v, want a failed write", err)
	}
}

// failWriter fails every write, as a full disk would.
type failWriter struct{}

var errFull = errors.New("disk full")

func (failWriter) Write([]byte) (int, error) { return 0, errFull }

func TestWriteZipReportsWriteFailures(t *testing.T) {
	// A small entry fits the zip writer's buffer, so the failure surfaces
	// when the archive is closed; a large one fails while it is written.
	if err := writeZip(failWriter{}, []entry{{name: "a", data: []byte("a")}}); !errors.Is(err, errFull) ||
		!strings.Contains(err.Error(), "write zip") {
		t.Errorf("small entry: error %v", err)
	}
	// Random bytes, because deflate would shrink anything regular to fit.
	big := make([]byte, 1<<20)
	_, _ = rand.NewChaCha8([32]byte{}).Read(big)
	if err := writeZip(failWriter{}, []entry{{name: "big", data: big}}); !errors.Is(err, errFull) ||
		!strings.Contains(err.Error(), "write big") {
		t.Errorf("large entry: error %v", err)
	}
}

func TestRun(t *testing.T) {
	bin, man := fixture(t, presenceManifest, peBinary)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-binary", bin, "-manifest", man, "-out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if want := filepath.Join(out, "weave-windows-presence_0.2.0_windows_amd64.zip"); strings.TrimSpace(stdout.String()) != want {
		t.Errorf("printed %q, want %q", stdout.String(), want)
	}

	for _, tc := range []struct {
		args []string
		code int
		text string
	}{
		{[]string{"-h"}, 0, "-binary"},
		{[]string{"-nope"}, 2, "not defined"},
		{[]string{"-binary", bin}, 2, "-binary and -manifest are required"},
		{[]string{"-binary", bin, "-manifest", man, "-arch", "386"}, 1, "modulezip: unsupported architecture"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(tc.args, &stdout, &stderr); code != tc.code || !strings.Contains(stderr.String(), tc.text) {
			t.Errorf("%q: exit %d, stderr %q; want %d and %q", tc.args, code, stderr.String(), tc.code, tc.text)
		}
	}
}

// The embedded scripts are the files in scripts/, and plain ASCII: Windows
// PowerShell reads a script with no byte-order mark in the system code page.
func TestScriptsAreTheFilesInScripts(t *testing.T) {
	for name, embedded := range map[string][]byte{"install.ps1": installScript, "uninstall.ps1": uninstallScript} {
		disk, err := os.ReadFile(filepath.Join("scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(disk, embedded) {
			t.Errorf("%s: embedded copy differs from scripts/%s", name, name)
		}
		for i, b := range embedded {
			if b > 0x7e || (b < 0x20 && b != '\n' && b != '\r' && b != '\t') {
				t.Errorf("%s: byte %d is 0x%02x, not printable ASCII", name, i, b)
				break
			}
		}
	}
}
