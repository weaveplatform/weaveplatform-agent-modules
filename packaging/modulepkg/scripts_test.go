package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeTools writes stand-ins for launchctl and pkgutil that log each call.
// launchctl reports the daemon loaded when loaded is set and fails every
// kill, which the scripts must swallow.
func fakeTools(t *testing.T, loaded bool) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "calls")
	state := "113" // launchctl's "could not find service"
	if loaded {
		state = "0"
	}
	tools := map[string]string{
		"launchctl": "#!/bin/sh\necho \"launchctl $*\" >> '" + log + "'\n" +
			"case \"$1\" in print) exit " + state + ";; *) echo refused >&2; exit 1;; esac\n",
		"pkgutil": "#!/bin/sh\necho \"pkgutil $*\" >> '" + log + "'\nexit 1\n",
	}
	for name, body := range tools {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, log
}

// runScript runs a script under sh with the stand-in tools, returning its
// exit code, its output and the tool calls it made.
func runScript(
	t *testing.T,
	script []byte,
	loaded bool,
	env []string,
	args ...string,
) (int, string, string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("no POSIX sh")
	}
	dir, log := fakeTools(t, loaded)
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, script, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, append([]string{path}, args...)...)
	cmd.Env = append(os.Environ(),
		"LAUNCHCTL="+filepath.Join(dir, "launchctl"), "PKGUTIL="+filepath.Join(dir, "pkgutil"))
	cmd.Env = append(cmd.Env, env...)
	out, _ := cmd.CombinedOutput()
	calls, _ := os.ReadFile(log)
	return cmd.ProcessState.ExitCode(), string(out), string(calls)
}

// postinstall reloads a loaded daemon, does nothing without one or on another
// volume, and never fails — the failing kill included.
func TestPostinstall(t *testing.T) {
	const hup = "launchctl kill HUP system/run.weaveplatform.agent"
	for name, c := range map[string]struct {
		loaded bool
		target string
		calls  []string
	}{
		"loaded":         {true, "/", []string{"launchctl print system/run.weaveplatform.agent", hup}},
		"not loaded":     {false, "/", []string{"launchctl print system/run.weaveplatform.agent"}},
		"another volume": {true, "/Volumes/Image", nil},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, calls := runScript(t, postinstall, c.loaded, nil, "pkg.pkg", "/", c.target)
			if code != 0 || out != "" {
				t.Fatalf("exit %d, output %q", code, out)
			}
			want := strings.Join(c.calls, "\n")
			if want != "" {
				want += "\n"
			}
			if calls != want {
				t.Fatalf("calls %q, want %q", calls, want)
			}
		})
	}
	// Run with no arguments, as by hand, the target is the running system.
	if code, _, calls := runScript(
		t,
		postinstall,
		true,
		nil,
	); code != 0 ||
		!strings.Contains(calls, hup) {
		t.Errorf("no arguments: exit %d, calls %q", code, calls)
	}
}

func TestPostinstallDryRun(t *testing.T) {
	code, out, calls := runScript(
		t,
		postinstall,
		true,
		[]string{"WEAVE_PKG_DRYRUN=1"},
		"p",
		"/",
		"/",
	)
	if code != 0 || !strings.HasSuffix(out, "kill HUP system/run.weaveplatform.agent\n") ||
		strings.Contains(calls, "kill") {
		t.Fatalf("exit %d, output %q, calls %q", code, out, calls)
	}
}

func TestUninstallDryRun(t *testing.T) {
	script := []byte(uninstaller("weave-macos-exec"))
	for _, loaded := range []bool{true, false} {
		code, out, calls := runScript(t, script, loaded, []string{"WEAVE_PKG_DRYRUN=1"})
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		mod := "/usr/local/libexec/weave/modules/weave-macos-exec"
		want := []string{
			"+ rm -f " + mod + "/weave-macos-exec " + mod + "/module.manifest.json",
			"+ rmdir " + mod,
			"+ " + "PKGUTIL --forget run.weaveplatform.module.weave-macos-exec",
		}
		if loaded {
			want = append(want, "+ LAUNCHCTL kill HUP system/run.weaveplatform.agent")
		}
		want = append(want,
			"+ rm -f /usr/local/libexec/weave/uninstall.d/weave-macos-exec.sh",
			"+ rmdir /usr/local/libexec/weave/uninstall.d",
			"weave-macos-exec: removed",
		)
		got := strings.Split(strings.TrimSpace(out), "\n")
		for i := range got {
			// The tool paths are the stand-ins'; name them by role.
			got[i] = replaceTool(got[i])
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf(
				"loaded %v: dry run\n%s\nwant\n%s",
				loaded,
				strings.Join(got, "\n"),
				strings.Join(want, "\n"),
			)
		}
		// A dry run only reads: the daemon check, never a change.
		if strings.Contains(calls, "kill") || strings.Contains(calls, "forget") {
			t.Errorf("a dry run made changes: %q", calls)
		}
	}
}

func replaceTool(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		switch filepath.Base(f) {
		case "launchctl":
			fields[i] = "LAUNCHCTL"
		case "pkgutil":
			fields[i] = "PKGUTIL"
		}
	}
	return strings.Join(fields, " ")
}

func TestUninstallRefuses(t *testing.T) {
	script := []byte(uninstaller("weave-macos-exec"))
	if code, out, _ := runScript(
		t,
		script,
		true,
		[]string{"WEAVE_PKG_DRYRUN=1"},
		"--purge",
	); code != 2 ||
		!strings.Contains(out, "usage") {
		t.Errorf("an argument: exit %d, %q", code, out)
	}
	if os.Geteuid() == 0 {
		return // root may run it for real, which this test must not do
	}
	if code, out, calls := runScript(
		t,
		script,
		true,
		nil,
	); code != 1 ||
		!strings.Contains(out, "run as root") ||
		calls != "" {
		t.Errorf("not root: exit %d, %q, calls %q", code, out, calls)
	}
}
