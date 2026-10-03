//go:build linux

package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// wlrRandr drives a wlroots compositor's outputs through
// wlr-output-management. A compositor without that protocol (GNOME's and
// KDE's do not implement it) is a session this module cannot configure.
type wlrRandr struct {
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// noProtocol is how wlr-randr reports a compositor without
// wlr-output-management.
const noProtocol = "wlr-output-management"

func (w wlrRandr) call(ctx context.Context, kind string, args ...string) ([]byte, error) {
	out, err := w.run(ctx, "wlr-randr", args...)
	if err != nil && strings.Contains(err.Error(), noProtocol) {
		return nil, unsupported(
			kind,
			"the Wayland compositor does not implement wlr-output-management",
		)
	}
	return out, err
}

func (w wlrRandr) list(ctx context.Context) ([]weavewire.DisplayInfo, error) {
	out, err := w.call(ctx, weavewire.KindDisplayList)
	if err != nil {
		return nil, err
	}
	return parseWlrRandr(string(out)), nil
}

// set applies a size, a scale or both in one call, so the compositor
// configures the output once. A listed size is chosen with --mode; anything
// else is a --custom-mode, which a virtual output accepts.
func (w wlrRandr) set(
	ctx context.Context,
	d weavewire.DisplayInfo,
	req weavewire.DisplaySetRequest,
) error {
	args := []string{"--output", d.ID}
	if req.Width != 0 {
		m := fmt.Sprintf("%dx%d", req.Width, req.Height)
		if req.RefreshHz != 0 {
			m += "@" + strconv.FormatFloat(req.RefreshHz, 'f', -1, 64) + "Hz"
		}
		flag := "--custom-mode"
		if listed(d, req) {
			flag = "--mode"
		}
		args = append(args, flag, m)
	}
	if req.Scale != 0 {
		args = append(args, "--scale", strconv.FormatFloat(req.Scale, 'f', -1, 64))
	}
	_, err := w.call(ctx, weavewire.KindDisplaySet, args...)
	return err
}

// parseWlrRandr reads wlr-randr's listing: an unindented header per output,
// then indented properties, with the modes indented further.
//
//	Virtual-1 "Red Hat, Inc. QEMU Monitor (Virtual-1)"
//	  Enabled: yes
//	  Modes:
//	    1920x1080 px, 60.000000 Hz (preferred, current)
//	  Scale: 2.000000
//
// Disabled outputs are left out, as xrandr's disconnected ones are. Wayland
// has no primary output; the service treats the first listed as the default.
func parseWlrRandr(out string) []weavewire.DisplayInfo {
	var (
		displays []weavewire.DisplayInfo
		cur      *weavewire.DisplayInfo
		enabled  []bool
	)
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			id, desc, _ := strings.Cut(line, " ")
			displays = append(
				displays,
				weavewire.DisplayInfo{ID: id, Name: strings.Trim(desc, `"`)},
			)
			enabled = append(enabled, true)
			cur = &displays[len(displays)-1]
			continue
		}
		if cur == nil {
			continue
		}
		key, val, _ := strings.Cut(strings.TrimSpace(line), ":")
		switch key {
		case "Enabled":
			enabled[len(enabled)-1] = strings.TrimSpace(val) == "yes"
		case "Scale":
			if s, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
				cur.Scale = s
			}
		default:
			if m, current, ok := parseWlrMode(line); ok {
				cur.Modes = append(cur.Modes, m)
				if current {
					cur.Current = m
				}
			}
		}
	}
	kept := displays[:0]
	for i, d := range displays {
		if enabled[i] {
			kept = append(kept, d)
		}
	}
	return kept
}

// parseWlrMode reads one mode line: "1920x1080 px, 60.000000 Hz (current)".
func parseWlrMode(line string) (weavewire.DisplayMode, bool, bool) {
	size, rest, ok := strings.Cut(strings.TrimSpace(line), " px,")
	if !ok {
		return weavewire.DisplayMode{}, false, false
	}
	w, h, ok := parseSize(size)
	if !ok {
		return weavewire.DisplayMode{}, false, false
	}
	m := weavewire.DisplayMode{Width: w, Height: h}
	if f := strings.Fields(rest); len(f) > 0 {
		if hz, err := strconv.ParseFloat(f[0], 64); err == nil {
			m.RefreshHz = hz
		}
	}
	return m, strings.Contains(rest, "current"), true
}
