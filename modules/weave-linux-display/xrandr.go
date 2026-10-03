//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// xrandr drives an X server's outputs.
type xrandr struct {
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (x xrandr) list(ctx context.Context) ([]weavewire.DisplayInfo, error) {
	out, err := x.run(ctx, "xrandr", "--query")
	if err != nil {
		return nil, err
	}
	return parseXrandr(string(out)), nil
}

// set applies a size to an output. X11 has no per-output UI scale — the
// desktop's scale is the toolkit's setting, and xrandr --scale only stretches
// the framebuffer — so a scale is refused before anything changes.
//
// A virtual GPU drives sizes it never listed, which is the case this exists
// for: an unlisted size is added to the output from cvt's CVT timings first.
func (x xrandr) set(
	ctx context.Context,
	d weavewire.DisplayInfo,
	req weavewire.DisplaySetRequest,
) error {
	if req.Scale != 0 {
		return &weavewire.UnsupportedError{
			Kind:   weavewire.KindDisplaySet,
			Reason: "X11 has no per-display UI scale; only a resolution can be set",
		}
	}
	size := fmt.Sprintf("%dx%d", req.Width, req.Height)
	if listed(d, req) {
		args := []string{"--output", d.ID, "--mode", size}
		if req.RefreshHz != 0 {
			args = append(args, "--rate", strconv.FormatFloat(req.RefreshHz, 'f', 2, 64))
		}
		_, err := x.run(ctx, "xrandr", args...)
		return err
	}

	cvtArgs := []string{strconv.Itoa(req.Width), strconv.Itoa(req.Height)}
	if req.RefreshHz != 0 {
		cvtArgs = append(cvtArgs, strconv.FormatFloat(req.RefreshHz, 'f', 2, 64))
	}
	out, err := x.run(ctx, "cvt", cvtArgs...)
	if err != nil {
		return err
	}
	name, timings, err := parseModeline(string(out))
	if err != nil {
		return err
	}
	// The mode may already exist on the server from an earlier set, added
	// to another output; newmode then fails with BadName, and addmode below
	// is what decides whether it can be used here.
	_, _ = x.run(ctx, "xrandr", append([]string{"--newmode", name}, timings...)...)
	if _, err := x.run(ctx, "xrandr", "--addmode", d.ID, name); err != nil {
		return err
	}
	_, err = x.run(ctx, "xrandr", "--output", d.ID, "--mode", name)
	return err
}

// parseXrandr reads `xrandr --query`: an output line per output, followed by
// its modes, one size per line with its refresh rates, '*' marking the
// current one and '+' the preferred.
//
//	Virtual-1 connected primary 1920x1080+0+0 (normal left ...) 0mm x 0mm
//	   1920x1080     60.00*+  59.96
//
// Disconnected outputs are left out: there is no display behind them. A
// connected output that is switched off is listed with no current mode.
func parseXrandr(out string) []weavewire.DisplayInfo {
	var (
		displays []weavewire.DisplayInfo
		cur      *weavewire.DisplayInfo
	)
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" || strings.HasPrefix(line, "Screen ") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			cur = nil
			f := strings.Fields(line)
			if len(f) < 2 || f[1] != "connected" {
				continue
			}
			displays = append(displays, weavewire.DisplayInfo{
				ID:      f[0],
				Primary: len(f) > 2 && f[2] == "primary",
			})
			cur = &displays[len(displays)-1]
			continue
		}
		if cur == nil {
			continue
		}
		f := strings.Fields(line)
		w, h, ok := parseSize(f[0])
		if !ok {
			continue
		}
		var last weavewire.DisplayMode
		for _, rate := range f[1:] {
			// A marker can stand apart from its rate ("60.00 +"), and
			// then belongs to the rate before it.
			if bare := strings.TrimRight(rate, "*+"); bare != "" {
				hz, err := strconv.ParseFloat(bare, 64)
				if err != nil {
					continue
				}
				last = weavewire.DisplayMode{Width: w, Height: h, RefreshHz: hz}
				// An interlaced size repeats a progressive one's numbers.
				if !slices.Contains(cur.Modes, last) {
					cur.Modes = append(cur.Modes, last)
				}
			}
			if strings.Contains(rate, "*") && last.Width != 0 {
				cur.Current = last
			}
		}
	}
	return displays
}

// parseSize reads WxH, ignoring an interlace suffix (1920x1080i).
func parseSize(s string) (int, int, bool) {
	ws, hs, ok := strings.Cut(strings.TrimRight(s, "i"), "x")
	if !ok {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(ws)
	h, err2 := strconv.Atoi(hs)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

var errNoModeline = errors.New("cvt printed no modeline")

// parseModeline reads cvt's output for the mode name and the timings
// xrandr --newmode takes after it:
//
//	Modeline "1920x1080_60.00"  173.00  1920 2048 2248 2576  1080 1083 1088 1120 -hsync +vsync
func parseModeline(out string) (string, []string, error) {
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 11 || f[0] != "Modeline" {
			continue
		}
		return strings.Trim(f[1], `"`), f[2:], nil
	}
	return "", nil, fmt.Errorf("%w: %q", errNoModeline, strings.TrimSpace(out))
}
