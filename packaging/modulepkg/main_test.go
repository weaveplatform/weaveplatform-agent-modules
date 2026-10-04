package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const presenceManifest = `{"schema":1,"id":"weave-macos-presence","version":"0.2.0","protocol":1,
"zone":"A","privilege":"service","session":"system",
"platforms":[{"os":"darwin","arch":"arm64"}]}`

// The test binary doubles as a stand-in pkgbuild: run with fakeEnv set, it
// records what it was asked to package — every argument, every file under
// --root and --scripts with its mode, and whether COPYFILE_DISABLE reached it
// — as JSON at the output path. It runs on every OS, Windows included, where
// there is neither a shell nor pkgbuild.
const fakeEnv = "MODULEPKG_FAKE_PKGBUILD"

type fakeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Data string `json:"data,omitempty"`
}

type fakePackage struct {
	Args     []string    `json:"args"`
	Copyfile string      `json:"copyfile"`
	Root     []fakeEntry `json:"root"`
	Scripts  []fakeEntry `json:"scripts"`
}

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeEnv); mode != "" {
		os.Exit(fakePkgbuild(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakePkgbuild(mode string, args []string) int {
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "pkgbuild: refused")
		return 1
	}
	flagValue := func(name string) string {
		i := slices.Index(args, name)
		if i < 0 || i+1 >= len(args) {
			return ""
		}
		return args[i+1]
	}
	walk := func(dir string) []fakeEntry {
		var out []fakeEntry
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, _ := d.Info()
			rel, _ := filepath.Rel(dir, p)
			e := fakeEntry{Path: filepath.ToSlash(rel), Mode: info.Mode().String()}
			if !d.IsDir() {
				b, _ := os.ReadFile(p)
				e.Data = string(b)
			}
			out = append(out, e)
			return nil
		})
		return out
	}
	pkg := fakePackage{
		Args:     args,
		Copyfile: os.Getenv("COPYFILE_DISABLE"),
		Root:     walk(flagValue("--root")),
		Scripts:  walk(flagValue("--scripts")),
	}
	b, _ := json.Marshal(pkg)
	if err := os.WriteFile(args[len(args)-1], b, 0o600); err != nil {
		return 1
	}
	return 0
}

// fake makes the test binary the pkgbuild build runs, in the given mode.
func fake(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv(fakeEnv, mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// fixture writes a module binary and manifest and returns their paths.
func fixture(t *testing.T, manifest string, binary []byte) (bin, man string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "weave-macos-presence")
	man = filepath.Join(dir, "module.manifest.json")
	if err := os.WriteFile(bin, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(man, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin, man
}

func readFake(t *testing.T, path string) fakePackage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p fakePackage
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildStagesThePayloadAndScripts(t *testing.T) {
	bin, man := fixture(t, presenceManifest, []byte("the binary"))
	out := filepath.Join(t.TempDir(), "dist")
	path, err := build(options{
		binary: bin, manifest: man, arch: "arm64", out: out,
		pkgbuild: fake(t, "ok"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "weave-macos-presence_0.2.0_darwin_arm64.pkg" {
		t.Fatalf("package file %s", filepath.Base(path))
	}
	p := readFake(t, path)

	if p.Copyfile != "1" {
		t.Error("pkgbuild ran without COPYFILE_DISABLE=1")
	}
	wantArgs := []string{
		"--identifier", "run.weaveplatform.module.weave-macos-presence", "--version", "0.2.0",
		"--install-location", "/", "--ownership", "recommended", path,
	}
	if got := p.Args[4:]; !slices.Equal(got, wantArgs) {
		t.Errorf("args %q, want %q after --root and --scripts", got, wantArgs)
	}

	dirs := []string{
		".", "usr", "usr/local", "usr/local/libexec", "usr/local/libexec/weave",
		"usr/local/libexec/weave/modules", "usr/local/libexec/weave/modules/weave-macos-presence",
		"usr/local/libexec/weave/uninstall.d",
	}
	files := map[string]struct {
		mode string
		data string
	}{
		"usr/local/libexec/weave/modules/weave-macos-presence/weave-macos-presence": {
			"-rwxr-xr-x", "the binary",
		},
		"usr/local/libexec/weave/modules/weave-macos-presence/module.manifest.json": {
			"-rw-r--r--", presenceManifest,
		},
		"usr/local/libexec/weave/uninstall.d/weave-macos-presence.sh": {
			"-rwxr-xr-x", uninstaller("weave-macos-presence"),
		},
	}
	var gotDirs []string
	for _, e := range p.Root {
		if strings.HasPrefix(e.Mode, "d") {
			gotDirs = append(gotDirs, e.Path)
			if runtime.GOOS != "windows" && e.Mode != "drwxr-xr-x" {
				t.Errorf("%s: mode %s, want 0755", e.Path, e.Mode)
			}
			continue
		}
		want, ok := files[e.Path]
		if !ok {
			t.Errorf("unexpected payload file %s", e.Path)
			continue
		}
		delete(files, e.Path)
		if e.Data != want.data {
			t.Errorf("%s holds %q", e.Path, e.Data)
		}
		if runtime.GOOS != "windows" && e.Mode != want.mode {
			t.Errorf("%s: mode %s, want %s", e.Path, e.Mode, want.mode)
		}
	}
	if len(files) != 0 {
		t.Errorf("payload lacks %v", files)
	}
	slices.Sort(gotDirs)
	slices.Sort(dirs)
	if !slices.Equal(gotDirs, dirs) {
		t.Errorf("payload directories %q, want %q", gotDirs, dirs)
	}

	if len(p.Scripts) != 2 || p.Scripts[1].Path != "postinstall" ||
		p.Scripts[1].Data != string(postinstall) {
		t.Fatalf("scripts %+v", p.Scripts)
	}
	if runtime.GOOS != "windows" && p.Scripts[1].Mode != "-rwxr-xr-x" {
		t.Errorf("postinstall mode %s", p.Scripts[1].Mode)
	}
}

func TestBuildSignsWithAnIdentity(t *testing.T) {
	bin, man := fixture(t, presenceManifest, []byte("x"))
	path, err := build(options{
		binary: bin, manifest: man, arch: "arm64", out: t.TempDir(),
		pkgbuild: fake(t, "ok"), sign: "Developer ID Installer: Example (TEAMID)",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := readFake(t, path).Args
	if i := slices.Index(
		args,
		"--sign",
	); i < 0 ||
		args[i+1] != "Developer ID Installer: Example (TEAMID)" {
		t.Errorf("args %q", args)
	}
}

func TestUninstallerNamesItsModule(t *testing.T) {
	s := uninstaller("weave-macos-exec")
	for _, want := range []string{
		"\nID=weave-macos-exec\n",
		"\nPKGID=run.weaveplatform.module.weave-macos-exec\n",
		"sudo /usr/local/libexec/weave/uninstall.d/weave-macos-exec.sh",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("uninstaller lacks %q", want)
		}
	}
	if strings.Contains(s, "@ID@") || strings.Contains(s, "@PKGID@") {
		t.Error("a placeholder is left in the uninstaller")
	}
}

func TestBuildRefuses(t *testing.T) {
	bin, man := fixture(t, presenceManifest, []byte("x"))
	write := func(content string) string {
		p := filepath.Join(t.TempDir(), "module.manifest.json")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ok := fake(t, "ok")
	cases := map[string]struct {
		o    options
		want error
		text string
	}{
		"arch": {options{binary: bin, manifest: man, arch: "riscv64"}, errArch, "riscv64"},
		"no manifest": {
			options{binary: bin, manifest: filepath.Join(t.TempDir(), "nope"), arch: "arm64"},
			nil,
			"nope",
		},
		"bad json": {
			options{binary: bin, manifest: write("{"), arch: "arm64"},
			errManifest,
			"unexpected end",
		},
		"bad id": {
			options{binary: bin, manifest: write(`{"id":"../x","version":"1.0.0"}`), arch: "arm64"},
			errManifest, "invalid module id",
		},
		"bad version": {
			options{binary: bin, manifest: write(`{"id":"m","version":"one"}`), arch: "arm64"},
			errManifest, "invalid version",
		},
		"linux module": {
			options{binary: bin, arch: "arm64", manifest: write(
				`{"id":"m","version":"1.0.0","platforms":[{"os":"linux","arch":"arm64"}]}`)},
			errNotDeclared, "does not declare darwin/arm64",
		},
		"undeclared arch": {
			options{binary: bin, manifest: man, arch: "amd64"},
			errNotDeclared,
			"darwin/amd64",
		},
		"no binary": {
			options{binary: filepath.Join(t.TempDir(), "nope"), manifest: man, arch: "arm64"},
			nil, "nope",
		},
		"no pkgbuild": {
			options{
				binary: bin, manifest: man, arch: "arm64",
				pkgbuild: filepath.Join(t.TempDir(), "pkgbuild"),
			},
			errNoPkgbuild, "built on macOS",
		},
		"out is a file": {
			options{
				binary:   bin,
				manifest: man,
				arch:     "arm64",
				pkgbuild: ok,
				out:      filepath.Join(file, "sub"),
			},
			nil,
			"output directory",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c.o.pkgbuild == "" {
				c.o.pkgbuild = ok
			}
			if c.o.out == "" {
				c.o.out = t.TempDir()
			}
			_, err := build(c.o)
			if err == nil || !strings.Contains(err.Error(), c.text) ||
				(c.want != nil && !errors.Is(err, c.want)) {
				t.Fatalf("err = %v, want %v naming %q", err, c.want, c.text)
			}
		})
	}
}

func TestBuildReportsAPkgbuildFailure(t *testing.T) {
	bin, man := fixture(t, presenceManifest, []byte("x"))
	_, err := build(options{
		binary: bin, manifest: man, arch: "arm64", out: t.TempDir(),
		pkgbuild: fake(t, "fail"),
	})
	if err == nil || !strings.Contains(err.Error(), "pkgbuild: refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildReportsAStagingFailure(t *testing.T) {
	bin, man := fixture(t, presenceManifest, []byte("x"))
	// A TMPDIR that does not exist leaves nowhere to stage.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("TMP", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("TEMP", filepath.Join(t.TempDir(), "absent"))
	_, err := build(options{
		binary: bin, manifest: man, arch: "arm64", out: t.TempDir(),
		pkgbuild: fake(t, "ok"),
	})
	if err == nil || !strings.Contains(err.Error(), "work directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestStageReportsEachFailure(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(plain, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stage(filepath.Join(plain, "root"), nil); err == nil {
		t.Error("staged under a file")
	}
	// A file where a directory of the payload should be.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "usr"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stage(root, []file{{name: "usr/local/x", mode: 0o644}}); err == nil {
		t.Error("staged through a file")
	}
	// A directory where a payload file should be.
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stage(root, []file{{name: "a/x", mode: 0o644}}); err == nil {
		t.Error("wrote a file over a directory")
	}
}

// A mode that cannot be set fails the build, for the payload and the
// scripts alike, rather than packaging a file at the umask's mode.
func TestBuildReportsAModeItCannotSet(t *testing.T) {
	bin, man := fixture(t, presenceManifest, []byte("x"))
	pkgbuild := fake(t, "ok")
	for _, failOn := range []string{"weave-macos-presence", "module.manifest.json", "postinstall", "scripts"} {
		t.Run(failOn, func(t *testing.T) {
			orig := chmod
			t.Cleanup(func() { chmod = orig })
			chmod = func(p string, m os.FileMode) error {
				if filepath.Base(p) == failOn {
					return errors.New("chmod refused")
				}
				return orig(p, m)
			}
			_, err := build(options{
				binary: bin, manifest: man, arch: "arm64", out: t.TempDir(),
				pkgbuild: pkgbuild,
			})
			if err == nil || !strings.Contains(err.Error(), "chmod refused") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestRun(t *testing.T) {
	bin, man := fixture(t, presenceManifest, []byte("x"))
	out := t.TempDir()
	pkgbuild := fake(t, "ok")
	var stdout, stderr bytes.Buffer
	args := []string{"-binary", bin, "-manifest", man, "-out", out, "-pkgbuild", pkgbuild}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	want := filepath.Join(out, "weave-macos-presence_0.2.0_darwin_arm64.pkg")
	if strings.TrimSpace(stdout.String()) != want {
		t.Fatalf("stdout %q, want %q (arm64 is the default)", stdout.String(), want)
	}

	for name, c := range map[string]struct {
		args []string
		code int
		want string
	}{
		"help":     {[]string{"-h"}, 0, "-binary"},
		"bad flag": {[]string{"-nope"}, 2, "-nope"},
		"missing":  {[]string{"-binary", bin}, 2, "required"},
		"build fail": {
			[]string{"-binary", bin, "-manifest", man, "-arch", "386", "-pkgbuild", pkgbuild},
			1, "unsupported architecture",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var o, e bytes.Buffer
			code := run(c.args, &o, &e)
			if code != c.code || !strings.Contains(e.String(), c.want) {
				t.Fatalf("code %d, stderr %q", code, e.String())
			}
		})
	}
}

// The real pkgbuild, on macOS: the package it writes holds exactly the
// payload, root:wheel at the staged modes, and the postinstall script, as
// installer would lay them down. Nothing is installed.
func TestBuildARealPackage(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("pkgbuild is macOS's")
	}
	for _, tool := range []string{"pkgbuild", "pkgutil", "lsbom"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
	}
	bin, man := fixture(t, presenceManifest, bytes.Repeat([]byte{0x7f}, 4096))
	path, err := build(options{
		binary: bin, manifest: man, arch: "arm64", out: t.TempDir(),
		pkgbuild: "pkgbuild",
	})
	if err != nil {
		t.Fatal(err)
	}

	files, err := exec.Command("pkgutil", "--payload-files", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(files)), "\n") {
		if !strings.Contains(line, "/._") { // a build machine's extended attributes
			listed = append(listed, line)
		}
	}
	want := []string{
		".", "./usr", "./usr/local", "./usr/local/libexec", "./usr/local/libexec/weave",
		"./usr/local/libexec/weave/modules",
		"./usr/local/libexec/weave/modules/weave-macos-presence",
		"./usr/local/libexec/weave/modules/weave-macos-presence/module.manifest.json",
		"./usr/local/libexec/weave/modules/weave-macos-presence/weave-macos-presence",
		"./usr/local/libexec/weave/uninstall.d",
		"./usr/local/libexec/weave/uninstall.d/weave-macos-presence.sh",
	}
	slices.Sort(listed)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Errorf(
			"payload files\n%s\nwant\n%s",
			strings.Join(listed, "\n"),
			strings.Join(want, "\n"),
		)
	}

	expanded := filepath.Join(t.TempDir(), "x")
	if out, err := exec.Command("pkgutil", "--expand", path, expanded).
		CombinedOutput(); err != nil {
		t.Fatalf("expand: %v: %s", err, out)
	}
	bom, err := exec.Command("lsbom", "-p", "MUGf", filepath.Join(expanded, "Bom")).Output()
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(bom)), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			t.Fatalf("bom line %q", line)
		}
		if f[1] != "root" || f[2] != "wheel" {
			t.Errorf("%s owned by %s:%s, want root:wheel", f[3], f[1], f[2])
		}
		modes[f[3]] = f[0]
	}
	for p, mode := range map[string]string{
		"./usr/local/libexec/weave/modules":                                           "drwxr-xr-x",
		"./usr/local/libexec/weave/modules/weave-macos-presence/weave-macos-presence": "-rwxr-xr-x",
		"./usr/local/libexec/weave/modules/weave-macos-presence/module.manifest.json": "-rw-r--r--",
		"./usr/local/libexec/weave/uninstall.d/weave-macos-presence.sh":               "-rwxr-xr-x",
	} {
		if modes[p] != mode {
			t.Errorf("%s: mode %q, want %s", p, modes[p], mode)
		}
	}
	script, err := os.ReadFile(filepath.Join(expanded, "Scripts", "postinstall"))
	if err != nil || !bytes.Equal(script, postinstall) {
		t.Errorf("postinstall in the package: %v", err)
	}
	info, err := os.ReadFile(filepath.Join(expanded, "PackageInfo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`identifier="run.weaveplatform.module.weave-macos-presence"`,
		`version="0.2.0"`, `install-location="/"`, `postinstall file="./postinstall"`,
	} {
		if !strings.Contains(string(info), want) {
			t.Errorf("PackageInfo lacks %s:\n%s", want, info)
		}
	}
}
