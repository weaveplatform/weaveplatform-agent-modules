package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The test binary doubles as a stand-in weavectl: run with fakeEnv set to
// "<mode>:<log>", it appends its arguments to the log and then succeeds, or
// fails as a weavectl with no core to talk to does.
const fakeEnv = "MODULEZIP_FAKE_WEAVECTL"

func TestMain(m *testing.M) {
	if v := os.Getenv(fakeEnv); v != "" {
		os.Exit(fakeWeavectl(v, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeWeavectl(v string, args []string) int {
	mode, log, _ := strings.Cut(v, ":")
	// The log path may hold a drive letter's colon; only the first one
	// separates the mode.
	f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 3
	}
	fmt.Fprintln(f, strings.Join(args, " "))
	f.Close()
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "weavectl: dial control socket: no such file")
		return 1
	}
	fmt.Println("reloaded: weave-windows-presence started")
	return 0
}

// shells are the PowerShells present: Windows PowerShell, which an
// unattended guest install runs, and PowerShell 7 wherever it is installed.
func shells(t *testing.T) []string {
	t.Helper()
	var found []string
	for _, name := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		t.Skip("no PowerShell")
	}
	return found
}

// unpack builds the presence package and unpacks it, as an operator would.
func unpack(t *testing.T) string {
	t.Helper()
	bin, man := fixture(t, presenceManifest, peBinary)
	path, err := build(options{binary: bin, manifest: man, arch: "amd64", out: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dir := t.TempDir()
	for _, f := range r.File {
		dst := filepath.Join(dir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
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
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type result struct {
	code   int
	out    string
	reload []string
}

// ps runs a script under shell with the stand-in weavectl in the given mode
// ("ok" or "fail"), returning its exit code, its output and weavectl's calls.
func ps(t *testing.T, shell, mode, script string, args ...string) result {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "weavectl.log")
	full := append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}, args...)
	full = append(full, "-WeaveCtl", exe)
	if runtime.GOOS != "windows" {
		// -ExecutionPolicy is a Windows notion; pwsh elsewhere warns of it.
		full = append(full[:2], full[4:]...)
	}
	cmd := exec.Command(shell, full...)
	cmd.Env = append(os.Environ(), fakeEnv+"="+mode+":"+log)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	var calls []string
	if b, err := os.ReadFile(log); err == nil {
		calls = strings.Fields(strings.TrimSpace(string(b)))
	}
	return result{code: code, out: string(out), reload: calls}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestInstallAndUninstall(t *testing.T) {
	for _, shell := range shells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			pkg := unpack(t)
			root := t.TempDir()
			moduleDir := filepath.Join(root, "modules", "weave-windows-presence")
			uninstaller := filepath.Join(root, "uninstall.d", "weave-windows-presence.ps1")

			r := ps(t, shell, "ok", filepath.Join(pkg, "install.ps1"), "-InstallDir", root)
			if r.code != 0 || !strings.Contains(r.out, "installed weave-windows-presence 0.2.0") {
				t.Fatalf("install exited %d: %s", r.code, r.out)
			}
			if strings.Join(r.reload, " ") != "reload" {
				t.Errorf("weavectl called with %q, want reload", r.reload)
			}
			for name, want := range map[string]string{
				filepath.Join(moduleDir, "weave-windows-presence.exe"): string(peBinary),
				filepath.Join(moduleDir, "module.manifest.json"):       presenceManifest,
				uninstaller: string(uninstallScript),
			} {
				if b, err := os.ReadFile(name); err != nil || string(b) != want {
					t.Errorf("%s: %q, %v", name, b, err)
				}
			}

			// An upgrade over an installed module replaces it in place.
			upgraded := filepath.Join(pkg, "module", "weave-windows-presence.exe")
			if err := os.WriteFile(upgraded, []byte("MZ the next version"), 0o644); err != nil {
				t.Fatal(err)
			}
			if r := ps(t, shell, "ok", filepath.Join(pkg, "install.ps1"), "-InstallDir", root); r.code != 0 {
				t.Fatalf("upgrade exited %d: %s", r.code, r.out)
			}
			if b, _ := os.ReadFile(filepath.Join(moduleDir, "weave-windows-presence.exe")); string(b) != "MZ the next version" {
				t.Errorf("upgrade left %q", b)
			}
			if entries, _ := os.ReadDir(moduleDir); len(entries) != 2 {
				t.Errorf("module directory holds %d entries after an upgrade, want 2", len(entries))
			}

			// An operator's config.json survives removal, and keeps the
			// directory.
			if err := os.WriteFile(filepath.Join(moduleDir, "config.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			r = ps(t, shell, "ok", uninstaller)
			if r.code != 0 || !strings.Contains(r.out, "removed weave-windows-presence") {
				t.Fatalf("uninstall exited %d: %s", r.code, r.out)
			}
			if strings.Join(r.reload, " ") != "reload" {
				t.Errorf("weavectl called with %q, want reload", r.reload)
			}
			for _, gone := range []string{
				filepath.Join(moduleDir, "weave-windows-presence.exe"),
				filepath.Join(moduleDir, "module.manifest.json"),
				uninstaller,
			} {
				if exists(t, gone) {
					t.Errorf("%s survived the uninstall", gone)
				}
			}
			if !exists(t, filepath.Join(moduleDir, "config.json")) {
				t.Error("the uninstall removed config.json")
			}
		})
	}
}

// Run from the package, uninstall.ps1 removes the package's module, and an
// emptied module directory goes too.
func TestUninstallFromThePackage(t *testing.T) {
	for _, shell := range shells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			pkg := unpack(t)
			root := t.TempDir()
			if r := ps(t, shell, "ok", filepath.Join(pkg, "install.ps1"), "-InstallDir", root, "-NoReload"); r.code != 0 ||
				len(r.reload) != 0 || !strings.Contains(r.out, "-NoReload") {
				t.Fatalf("install -NoReload exited %d, reloaded %q: %s", r.code, r.reload, r.out)
			}
			r := ps(t, shell, "ok", filepath.Join(pkg, "uninstall.ps1"), "-InstallDir", root, "-NoReload")
			if r.code != 0 || len(r.reload) != 0 {
				t.Fatalf("uninstall exited %d, reloaded %q: %s", r.code, r.reload, r.out)
			}
			if exists(t, filepath.Join(root, "modules", "weave-windows-presence")) {
				t.Error("the emptied module directory survived")
			}
		})
	}
}

// Neither script fails because weave-agent could not be told: the module is
// in place, or gone, either way.
func TestReloadFailuresOnlyWarn(t *testing.T) {
	for _, shell := range shells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			pkg := unpack(t)
			root := t.TempDir()
			for _, script := range []string{"install.ps1", "uninstall.ps1"} {
				r := ps(t, shell, "fail", filepath.Join(pkg, script), "-InstallDir", root)
				if r.code != 0 || !strings.Contains(r.out, "weavectl reload exited 1") ||
					!strings.Contains(r.out, "no such file") {
					t.Errorf("%s with a failing weavectl exited %d: %s", script, r.code, r.out)
				}
			}
			// No weavectl at all: core is not installed, and finds the
			// module when it is.
			exe := []string{"-NoProfile", "-NonInteractive", "-File", filepath.Join(pkg, "install.ps1"),
				"-InstallDir", root}
			out, err := exec.Command(shell, exe...).CombinedOutput()
			if err != nil || !strings.Contains(string(out), "no weavectl at") {
				t.Errorf("install with no weavectl: %v: %s", err, out)
			}
		})
	}
}

func TestInstallRefusesABrokenPackage(t *testing.T) {
	for _, shell := range shells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			pkg := unpack(t)
			if err := os.Remove(filepath.Join(pkg, "module", "weave-windows-presence.exe")); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			r := ps(t, shell, "ok", filepath.Join(pkg, "install.ps1"), "-InstallDir", root)
			if r.code != 1 || !strings.Contains(r.out, `has no module`) {
				t.Errorf("install exited %d: %s", r.code, r.out)
			}
			if len(r.reload) != 0 {
				t.Errorf("a failed install reloaded weave-agent: %q", r.reload)
			}

			// An id that could climb out of the modules tree is refused.
			man := filepath.Join(pkg, "module", "module.manifest.json")
			if err := os.WriteFile(man, []byte(`{"id":"..\\..\\x","version":"0.2.0"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, script := range []string{"install.ps1", "uninstall.ps1"} {
				r := ps(t, shell, "ok", filepath.Join(pkg, script), "-InstallDir", root)
				if r.code != 1 || !strings.Contains(r.out, "invalid module id") {
					t.Errorf("%s exited %d: %s", script, r.code, r.out)
				}
			}
		})
	}
}
