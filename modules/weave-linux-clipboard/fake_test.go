//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// The fake tools stand in for wl-paste and wl-copy, so the exec paths
// run for real without a display server — and without ever touching the
// clipboard of a machine that has one. They keep the "clipboard" in a
// directory: targets lists what is offered, data/<key> holds each target's
// bytes, and a file named fail makes every call fail.
const fakeWlPaste = `#!/bin/sh
S="$FAKE_CLIP"
[ -f "$S/fail" ] && exit 2
case "$1" in
--list-types)
	[ -s "$S/targets" ] || { echo "Nothing is copied" >&2; exit 1; }
	cat "$S/targets" ;;
--no-newline)
	k=$(printf '%s' "$3" | tr '/;= ' '____')
	[ -f "$S/data/$k" ] || exit 1
	cat "$S/data/$k" ;;
*) exit 64 ;;
esac
`

const fakeWlCopy = `#!/bin/sh
S="$FAKE_CLIP"
[ -f "$S/fail" ] && exit 2
k=$(printf '%s' "$2" | tr '/;= ' '____')
rm -rf "$S/data"; mkdir -p "$S/data"
cat > "$S/data/$k"
printf '%s\n' "$2" > "$S/targets"
`

// fakeClip is the fake tools' clipboard.
type fakeClip struct{ dir string }

// installFake puts the fake tools first on PATH and returns their clipboard.
func installFake(t *testing.T) fakeClip {
	t.Helper()
	bin := t.TempDir()
	for name, script := range map[string]string{
		"wl-paste": fakeWlPaste, "wl-copy": fakeWlCopy,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := fakeClip{dir: t.TempDir()}
	t.Setenv("FAKE_CLIP", f.dir)
	return f
}

func key(target string) string {
	return strings.NewReplacer("/", "_", ";", "_", "=", "_", " ", "_").Replace(target)
}

// offer replaces the fake clipboard's content with targets and their data.
func (f fakeClip) offer(t *testing.T, extra []string, data map[string]string) {
	t.Helper()
	dir := filepath.Join(f.dir, "data")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	targets := append([]string{}, extra...)
	for target, d := range data {
		targets = append(targets, target)
		if err := os.WriteFile(filepath.Join(dir, key(target)), []byte(d), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.write(t, "targets", strings.Join(targets, "\n")+"\n")
}

func (f fakeClip) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// held reads what the fake clipboard holds for target.
func (f fakeClip) held(t *testing.T, target string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "data", key(target)))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (f fakeClip) fail(t *testing.T) { f.write(t, "fail", "") }

// toolClipboard builds the backend as core would start it in a Wayland
// session whose compositor has no data control and that has no X display:
// through the wl-clipboard stand-ins.
func toolClipboard(t *testing.T) *clipboard {
	t.Helper()
	c := testClipboard(
		map[string]string{"WAYLAND_DISPLAY": "wayland-0", "XDG_RUNTIME_DIR": t.TempDir()},
	)
	t.Cleanup(c.closeMechanism)
	if _, err := c.mechanism(weavewire.KindClipboardStat); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.m.(*tool); !ok {
		t.Fatalf("mechanism %T, want the tools", c.m)
	}
	return c
}

// testClipboard is the backend in a session with env, whose display servers
// refuse it unless a test dials them itself.
func testClipboard(env map[string]string) *clipboard {
	c := newClipboard()
	c.getenv = func(k string) string { return env[k] }
	c.dialWayland = func(string) (mechanism, error) { return nil, errNoDataControl }
	c.dialX11 = func(string) (mechanism, error) { return nil, errNoXFixes }
	return c
}

// closeMechanism closes the backend's connection, for a test's cleanup.
func (c *clipboard) closeMechanism() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m != nil {
		c.m.close()
		c.m = nil
	}
}
