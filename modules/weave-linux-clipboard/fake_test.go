//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake tools stand in for wl-paste, wl-copy and xclip, so the exec paths
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

const fakeXclip = `#!/bin/sh
S="$FAKE_CLIP"
[ -f "$S/fail" ] && exit 2
t="$4"
k=$(printf '%s' "$t" | tr '/;= ' '____')
case "$5" in
-o)
	if [ "$t" = TARGETS ]; then
		[ -s "$S/targets" ] || { echo "Error: target TARGETS not available" >&2; exit 1; }
		cat "$S/targets"; exit 0
	fi
	[ -f "$S/data/$k" ] || exit 1
	cat "$S/data/$k" ;;
-i)
	rm -rf "$S/data"; mkdir -p "$S/data"
	cat > "$S/data/$k"
	printf 'TARGETS\n%s\n' "$t" > "$S/targets" ;;
*) exit 64 ;;
esac
`

// fakeClip is the fake tools' clipboard.
type fakeClip struct{ dir string }

// installFake puts the fake tools first on PATH and returns their clipboard.
func installFake(t *testing.T) fakeClip {
	t.Helper()
	bin := t.TempDir()
	for name, script := range map[string]string{
		"wl-paste": fakeWlPaste, "wl-copy": fakeWlCopy, "xclip": fakeXclip,
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

// wayland and x11 build the backend as core would start it in each kind of
// session.
func wayland(t *testing.T) *clipboard {
	t.Helper()
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("DISPLAY", "")
	c := newClipboard()
	if c.tool == nil {
		t.Fatalf("no tool: %s", c.missing)
	}
	return c
}

func x11(t *testing.T) *clipboard {
	t.Helper()
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":0")
	c := newClipboard()
	if c.tool == nil {
		t.Fatalf("no tool: %s", c.missing)
	}
	return c
}
