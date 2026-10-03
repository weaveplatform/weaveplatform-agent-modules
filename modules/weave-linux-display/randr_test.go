//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// fakeScript stands in for xrandr, wlr-randr and cvt: it records its argv,
// prints a fixture for a listing, and fails any call whose arguments contain
// FAKE_FAIL. No test runs the real tools, so no test changes a display.
const fakeScript = `#!/bin/sh
# The module sees only the fakes on PATH; the script itself needs the system.
PATH=/usr/bin:/bin
echo "$(basename "$0") $*" >> "$FAKE_LOG"
if [ -n "$FAKE_FAIL" ]; then
  case "$(basename "$0") $*" in *"$FAKE_FAIL"*) echo "$FAKE_STDERR" >&2; exit 1;; esac
fi
case "$(basename "$0") $*" in
  "xrandr --query"|"wlr-randr ") cat "$FAKE_LIST";;
  cvt*) cat "$FAKE_CVT";;
esac
`

// session puts fake tools on an otherwise empty PATH and sets the session
// environment core would give the module.
func session(t *testing.T, env map[string]string, tools ...string) func() []string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(fakeScript), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "log")
	abs, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	defaults := map[string]string{
		"PATH": dir, "FAKE_LOG": log, "FAKE_FAIL": "", "FAKE_STDERR": "refused",
		"WAYLAND_DISPLAY": "", "DISPLAY": "", "FAKE_CVT": filepath.Join(abs, "cvt.txt"),
	}
	for k, v := range env {
		defaults[k] = v
	}
	if defaults["FAKE_LIST"] == "" {
		defaults["FAKE_LIST"] = filepath.Join(abs, "xrandr-query.txt")
	}
	for k, v := range defaults {
		t.Setenv(k, v)
	}
	return func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func wlrList(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("testdata/wlr-randr.txt")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func isUnsupported(err error) bool {
	_, ok := errors.AsType[*weavewire.UnsupportedError](err)
	return ok
}

func TestNoDisplayServerIsUnsupported(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no session env":        {},
		"Wayland, no wlr-randr": {"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"},
		"X11, no xrandr":        {"DISPLAY": ":0"},
	} {
		t.Run(name, func(t *testing.T) {
			// xrandr is present but must not be used under Wayland.
			session(t, env, "xrandr")
			if name == "X11, no xrandr" {
				t.Setenv("PATH", t.TempDir())
			}
			r := newRandr()
			if _, err := r.List(context.Background()); !isUnsupported(err) {
				t.Errorf("list: err = %v, want unsupported", err)
			}
			_, err := r.Set(
				context.Background(),
				weavewire.DisplaySetRequest{DisplayID: "Virtual-1", Scale: 2},
			)
			if !isUnsupported(err) {
				t.Errorf("set: err = %v, want unsupported", err)
			}
		})
	}
}

func TestXrandrListAndSetAListedMode(t *testing.T) {
	calls := session(t, map[string]string{"DISPLAY": ":0"}, "xrandr", "cvt")
	r := newRandr()
	got, err := r.List(context.Background())
	if err != nil || len(got) != 3 || !got[0].Primary {
		t.Fatalf("list = %+v, %v", got, err)
	}
	if _, err := r.Set(context.Background(), weavewire.DisplaySetRequest{
		DisplayID: "Virtual-1", Width: 1280, Height: 1024, RefreshHz: 75.02,
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"xrandr --query", "xrandr --query",
		"xrandr --output Virtual-1 --mode 1280x1024 --rate 75.02",
		"xrandr --query",
	}
	if got := calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls\n%q\nwant\n%q", got, want)
	}
}

func TestXrandrAddsAnUnlistedMode(t *testing.T) {
	calls := session(t, map[string]string{"DISPLAY": ":0"}, "xrandr", "cvt")
	if _, err := newRandr().Set(context.Background(), weavewire.DisplaySetRequest{
		DisplayID: "Virtual-1", Width: 1600, Height: 900, RefreshHz: 60,
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"xrandr --query",
		"cvt 1600 900 60.00",
		"xrandr --newmode 1600x900_60.00 118.25 1600 1696 1856 2112 900 903 908 934 -hsync +vsync",
		"xrandr --addmode Virtual-1 1600x900_60.00",
		"xrandr --output Virtual-1 --mode 1600x900_60.00",
		"xrandr --query",
	}
	if got := calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls\n%q\nwant\n%q", got, want)
	}
}

func TestXrandrSetFailures(t *testing.T) {
	ctx := context.Background()
	req := weavewire.DisplaySetRequest{DisplayID: "Virtual-1", Width: 1600, Height: 900}

	calls := session(t, map[string]string{"DISPLAY": ":0"}, "xrandr", "cvt")
	_, err := newRandr().Set(ctx, weavewire.DisplaySetRequest{DisplayID: "Virtual-1", Width: 1024, Height: 768, Scale: 2})
	if !isUnsupported(err) {
		t.Errorf("scale on X11: err = %v, want unsupported", err)
	}
	if got := calls(); len(got) != 1 {
		t.Errorf("a refused scale still ran %q", got)
	}

	for pattern, why := range map[string]string{
		"--query":   "listing",
		"cvt":       "computing the timings",
		"--addmode": "adding the mode",
		"--output":  "switching to it",
	} {
		session(t, map[string]string{"DISPLAY": ":0", "FAKE_FAIL": pattern}, "xrandr", "cvt")
		if _, err := newRandr().Set(ctx, req); err == nil ||
			!strings.Contains(err.Error(), "refused") {
			t.Errorf("%s failure: err = %v", why, err)
		}
	}

	// newmode fails when the mode already exists; addmode decides.
	calls = session(
		t,
		map[string]string{"DISPLAY": ":0", "FAKE_FAIL": "--newmode"},
		"xrandr",
		"cvt",
	)
	if _, err := newRandr().Set(ctx, req); err != nil {
		t.Errorf("an existing mode: err = %v", err)
	}

	session(t, map[string]string{"DISPLAY": ":0", "FAKE_CVT": "/dev/null"}, "xrandr", "cvt")
	if _, err := newRandr().Set(ctx, req); !errors.Is(err, errNoModeline) {
		t.Errorf("cvt without a modeline: err = %v", err)
	}

	session(t, map[string]string{"DISPLAY": ":0"}, "xrandr", "cvt")
	if _, err := newRandr().Set(ctx, weavewire.DisplaySetRequest{DisplayID: "gone", Scale: 0, Width: 1, Height: 1}); !errors.Is(
		err,
		errGone,
	) {
		t.Errorf("an unknown output: err = %v", err)
	}
}

func TestWlrRandrListAndSet(t *testing.T) {
	ctx := context.Background()
	env := map[string]string{"WAYLAND_DISPLAY": "wayland-1", "FAKE_LIST": wlrList(t)}
	calls := session(t, env, "wlr-randr", "xrandr")
	r := newRandr()
	got, err := r.List(ctx)
	if err != nil || len(got) != 2 || got[0].Scale != 2 {
		t.Fatalf("list = %+v, %v", got, err)
	}
	for _, req := range []weavewire.DisplaySetRequest{
		{DisplayID: "Virtual-1", Width: 1920, Height: 1080},
		{DisplayID: "Virtual-1", Width: 1600, Height: 900, RefreshHz: 59.94},
		{DisplayID: "HDMI-A-1", Scale: 1.25},
		{DisplayID: "HDMI-A-1", Width: 3840, Height: 2160, RefreshHz: 30, Scale: 2},
	} {
		if _, err := r.Set(ctx, req); err != nil {
			t.Fatalf("set %+v: %v", req, err)
		}
	}
	var sets []string
	for _, c := range calls() {
		if strings.TrimSpace(c) != "wlr-randr" {
			sets = append(sets, c)
		}
	}
	want := []string{
		"wlr-randr --output Virtual-1 --mode 1920x1080",
		"wlr-randr --output Virtual-1 --custom-mode 1600x900@59.94Hz",
		"wlr-randr --output HDMI-A-1 --scale 1.25",
		"wlr-randr --output HDMI-A-1 --mode 3840x2160@30Hz --scale 2",
	}
	if strings.Join(sets, "|") != strings.Join(want, "|") {
		t.Errorf("sets\n%q\nwant\n%q", sets, want)
	}
}

func TestWlrRandrWithoutTheProtocolIsUnsupported(t *testing.T) {
	ctx := context.Background()
	session(t, map[string]string{
		"WAYLAND_DISPLAY": "wayland-0", "FAKE_LIST": wlrList(t), "FAKE_FAIL": "wlr-randr",
		"FAKE_STDERR": "compositor doesn't support wlr-output-management-unstable-v1",
	}, "wlr-randr")
	if _, err := newRandr().List(ctx); !isUnsupported(err) {
		t.Errorf("list: err = %v, want unsupported", err)
	}

	session(t, map[string]string{
		"WAYLAND_DISPLAY": "wayland-0", "FAKE_LIST": wlrList(t), "FAKE_FAIL": "--scale",
	}, "wlr-randr")
	_, err := newRandr().Set(ctx, weavewire.DisplaySetRequest{DisplayID: "Virtual-1", Scale: 3})
	if err == nil || isUnsupported(err) {
		t.Errorf("a refused scale: err = %v, want the tool's failure", err)
	}
}
