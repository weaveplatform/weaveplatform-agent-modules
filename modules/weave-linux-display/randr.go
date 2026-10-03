//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// randr is the Linux display backend. Linux has no display API below the
// display server, so it drives the session's own tool: wlr-randr speaks
// wlr-output-management to a wlroots compositor, and xrandr speaks RandR to
// an X server. Both run in the console user's session with the environment
// core launched the module with.
type randr struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	run      func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func newRandr() randr {
	return randr{getenv: os.Getenv, lookPath: exec.LookPath, run: runTool}
}

// runTool runs a display tool and returns its output; a failure carries the
// tool's own complaint, which is the only explanation there is.
func runTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// tool is one display server's way of listing and setting outputs.
type tool interface {
	list(ctx context.Context) ([]weavewire.DisplayInfo, error)
	set(ctx context.Context, d weavewire.DisplayInfo, req weavewire.DisplaySetRequest) error
}

// pick chooses the tool for the session.
//
// A Wayland session is driven only through Wayland: its XWayland display
// answers xrandr with emulated outputs whose changes the compositor ignores,
// so falling back to xrandr there would report success for nothing.
func (r randr) pick(kind string) (tool, error) {
	if wl := r.getenv("WAYLAND_DISPLAY"); wl != "" {
		if _, err := r.lookPath("wlr-randr"); err != nil {
			return nil, unsupported(kind, fmt.Sprintf(
				"Wayland session %s has no wlr-randr; displays are configurable only on a "+
					"compositor with wlr-output-management, through wlr-randr", wl))
		}
		return wlrRandr{run: r.run}, nil
	}
	if x := r.getenv("DISPLAY"); x != "" {
		if _, err := r.lookPath("xrandr"); err != nil {
			return nil, unsupported(kind, fmt.Sprintf("X11 session %s has no xrandr", x))
		}
		return xrandr{run: r.run}, nil
	}
	return nil, unsupported(
		kind,
		"the session has neither WAYLAND_DISPLAY nor DISPLAY: no display server to ask",
	)
}

// unsupported is the answer for a session this module cannot configure. It is
// the same for list and set, so a host feature-gates on either.
func unsupported(kind, reason string) error {
	return &weavewire.UnsupportedError{Kind: kind, Reason: reason}
}

// List reports the session's enabled outputs.
func (r randr) List(ctx context.Context) ([]weavewire.DisplayInfo, error) {
	t, err := r.pick(weavewire.KindDisplayList)
	if err != nil {
		return nil, err
	}
	return t.list(ctx)
}

var errGone = errors.New("the output is no longer listed")

// Set changes one output and reports it as the tool lists it afterwards.
func (r randr) Set(
	ctx context.Context,
	req weavewire.DisplaySetRequest,
) (weavewire.DisplayInfo, error) {
	t, err := r.pick(weavewire.KindDisplaySet)
	if err != nil {
		return weavewire.DisplayInfo{}, err
	}
	before, err := find(ctx, t, req.DisplayID)
	if err != nil {
		return weavewire.DisplayInfo{}, err
	}
	if err := t.set(ctx, before, req); err != nil {
		return weavewire.DisplayInfo{}, err
	}
	return find(ctx, t, req.DisplayID)
}

func find(ctx context.Context, t tool, id string) (weavewire.DisplayInfo, error) {
	displays, err := t.list(ctx)
	if err != nil {
		return weavewire.DisplayInfo{}, err
	}
	for _, d := range displays {
		if d.ID == id {
			return d, nil
		}
	}
	return weavewire.DisplayInfo{}, fmt.Errorf("%s: %w", id, errGone)
}

// listed reports whether d advertises a mode of the requested size (and
// refresh rate, when one is asked for).
func listed(d weavewire.DisplayInfo, req weavewire.DisplaySetRequest) bool {
	for _, m := range d.Modes {
		if m.Width == req.Width && m.Height == req.Height &&
			(req.RefreshHz == 0 || nearHz(m.RefreshHz, req.RefreshHz)) {
			return true
		}
	}
	return false
}

func nearHz(a, b float64) bool { return a-b < 0.5 && b-a < 0.5 }
