//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// mode is one CoreGraphics display mode: its size in points (what the desktop
// lays out in) and in pixels (what the panel or virtual GPU drives).
type mode struct {
	width, height  int // points
	pixelW, pixelH int
	refresh        float64
	usable         bool // usable for the desktop GUI
	handle         any  // the CGDisplayModeRef to apply; opaque here
}

// scale is the backing scale of the mode: 2 for a Retina mode, 1 otherwise.
func (m mode) scale() float64 {
	if m.width <= 0 {
		return 0
	}
	return float64(m.pixelW) / float64(m.width)
}

func (m mode) wire() weavewire.DisplayMode {
	return weavewire.DisplayMode{Width: m.pixelW, Height: m.pixelH, RefreshHz: m.refresh}
}

// quartz is the CoreGraphics surface the backend needs. Each call is a field
// so tests drive the selection logic with any set of modes and never apply a
// mode to the display of the machine running them.
type quartz struct {
	active  func() ([]uint32, error)
	isMain  func(id uint32) bool
	current func(id uint32) (mode, bool)
	modes   func(id uint32) []mode
	apply   func(id uint32, m mode) error
}

// displays is the macOS display backend.
type displays struct{ q quartz }

func newDisplays() displays { return displays{q: coreGraphics()} }

// errNoMode reports a size, scale or refresh rate the display offers no mode
// for. macOS cannot drive a mode the window server does not list: unlike X11
// there is no way to add one from a user session.
var errNoMode = errors.New("the display offers no mode")

// List reports every active display.
func (d displays) List(context.Context) ([]weavewire.DisplayInfo, error) {
	ids, err := d.q.active()
	if err != nil {
		return nil, err
	}
	out := make([]weavewire.DisplayInfo, 0, len(ids))
	for _, id := range ids {
		info, ok := d.info(id)
		if ok {
			out = append(out, info)
		}
	}
	return out, nil
}

// info describes one display; false when it went away mid-read.
func (d displays) info(id uint32) (weavewire.DisplayInfo, bool) {
	cur, ok := d.q.current(id)
	if !ok {
		return weavewire.DisplayInfo{}, false
	}
	info := weavewire.DisplayInfo{
		ID:      strconv.FormatUint(uint64(id), 10),
		Primary: d.q.isMain(id),
		Current: cur.wire(),
		Scale:   cur.scale(),
	}
	// Modes are listed in pixels, so a Retina mode and a 1x mode of the same
	// pixel size are one entry: the scale is a separate axis of a set.
	for _, m := range d.q.modes(id) {
		w := m.wire()
		if m.usable && !slices.Contains(info.Modes, w) {
			info.Modes = append(info.Modes, w)
		}
	}
	return info, true
}

// Set switches the display to the mode that best matches req. The service has
// already resolved req.DisplayID to a listed display.
func (d displays) Set(
	_ context.Context,
	req weavewire.DisplaySetRequest,
) (weavewire.DisplayInfo, error) {
	id64, err := strconv.ParseUint(req.DisplayID, 10, 32)
	if err != nil {
		return weavewire.DisplayInfo{}, fmt.Errorf("display id %q: %w", req.DisplayID, err)
	}
	id := uint32(id64)
	cur, ok := d.q.current(id)
	if !ok {
		return weavewire.DisplayInfo{}, fmt.Errorf("display %d: %w", id, errGone)
	}
	m, err := choose(d.q.modes(id), cur, req)
	if err != nil {
		return weavewire.DisplayInfo{}, err
	}
	if err := d.q.apply(id, m); err != nil {
		return weavewire.DisplayInfo{}, err
	}
	info, ok := d.info(id)
	if !ok {
		return weavewire.DisplayInfo{}, fmt.Errorf("display %d: %w", id, errGone)
	}
	return info, nil
}

var errGone = errors.New("the display is no longer active")

// choose picks the mode for req among a display's modes.
//
// The size is in pixels and defaults to the current one, so a request for
// only a scale keeps the resolution and changes how the desktop is laid out
// on it. What the request leaves open — scale, refresh — prefers the current
// value, so resizing a window does not also flip Retina off or change the
// refresh rate.
func choose(modes []mode, cur mode, req weavewire.DisplaySetRequest) (mode, error) {
	w, h := req.Width, req.Height
	if w == 0 {
		w, h = cur.pixelW, cur.pixelH
	}
	best, bestScore := mode{}, -1
	for _, m := range modes {
		if !m.usable || m.pixelW != w || m.pixelH != h {
			continue
		}
		if req.Scale != 0 && !near(m.scale(), req.Scale, 0.01) {
			continue
		}
		if req.RefreshHz != 0 && !near(m.refresh, req.RefreshHz, 0.5) {
			continue
		}
		score := 0
		if near(m.scale(), cur.scale(), 0.01) {
			score += 2
		}
		if near(m.refresh, cur.refresh, 0.5) {
			score++
		}
		if score > bestScore {
			best, bestScore = m, score
		}
	}
	if bestScore < 0 {
		return mode{}, fmt.Errorf(
			"%w for %dx%d at scale %g, %g Hz",
			errNoMode,
			w,
			h,
			req.Scale,
			req.RefreshHz,
		)
	}
	return best, nil
}

func near(a, b, tolerance float64) bool { return math.Abs(a-b) <= tolerance }
