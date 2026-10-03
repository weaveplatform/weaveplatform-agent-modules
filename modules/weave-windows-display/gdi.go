//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"syscall"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/graphics/gdi"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// baseDPI is 100% scale on Windows.
const baseDPI = 96

// win32 is the User32 surface the backend needs. Each call is a field so tests
// drive the backend with any set of devices and modes, and never change the
// display mode of the machine running them.
type win32 struct {
	// device enumerates display devices: adapters when parent is nil, the
	// monitors on one adapter otherwise.
	device func(parent *string, i uint32) (gdi.DISPLAY_DEVICEW, bool)
	// settings reads a device's current mode (ENUM_CURRENT_SETTINGS) or
	// its i-th mode.
	settings func(name string, i gdi.ENUM_DISPLAY_SETTINGS_MODE) (gdi.DEVMODEW, bool)
	// dpi is the effective DPI of the monitor at a desktop point.
	dpi func(x, y int32) (uint32, error)
	// change is ChangeDisplaySettingsEx.
	change func(name string, dm *gdi.DEVMODEW, flags gdi.CDS_TYPE) gdi.DISP_CHANGE
	// aware makes the calling thread per-monitor DPI aware until the
	// returned function restores it.
	aware func() func()
}

// displays is the Windows display backend.
type displays struct {
	w win32
	// flags is what a set passes ChangeDisplaySettingsEx: 0, a dynamic
	// change that is not saved to the user's profile, because the host
	// re-asserts its size on every resize of its window and should not
	// overwrite the user's saved resolution. Tests pass CDS_TEST, which
	// validates a mode without applying it.
	flags gdi.CDS_TYPE
}

func newGDI() displays { return displays{w: user32()} }

// List reports every display attached to the desktop.
func (d displays) List(context.Context) ([]weavewire.DisplayInfo, error) {
	defer d.w.aware()()
	var out []weavewire.DisplayInfo
	for i := uint32(0); ; i++ {
		dd, ok := d.w.device(nil, i)
		if !ok {
			break
		}
		if dd.StateFlags&gdi.DISPLAY_DEVICE_ATTACHED_TO_DESKTOP == 0 {
			continue // an adapter output with nothing on it
		}
		if info, ok := d.describe(dd); ok {
			out = append(out, info)
		}
	}
	return out, nil
}

func (d displays) describe(dd gdi.DISPLAY_DEVICEW) (weavewire.DisplayInfo, bool) {
	name := syscall.UTF16ToString(dd.DeviceName[:])
	cur, ok := d.w.settings(name, gdi.ENUM_CURRENT_SETTINGS)
	if !ok {
		return weavewire.DisplayInfo{}, false
	}
	info := weavewire.DisplayInfo{
		ID:      name,
		Name:    syscall.UTF16ToString(dd.DeviceString[:]),
		Primary: dd.StateFlags&gdi.DISPLAY_DEVICE_PRIMARY_DEVICE != 0,
		Current: modeOf(cur),
	}
	// The monitor's own name ("Generic PnP Monitor") says more than the
	// adapter's when there is one.
	if mon, ok := d.w.device(&name, 0); ok {
		if s := syscall.UTF16ToString(mon.DeviceString[:]); s != "" {
			info.Name = s
		}
	}
	// Modes differ in colour depth too; a host chooses a size and a rate.
	for i := gdi.ENUM_DISPLAY_SETTINGS_MODE(0); ; i++ {
		dm, ok := d.w.settings(name, i)
		if !ok {
			break
		}
		if m := modeOf(dm); !slices.Contains(info.Modes, m) {
			info.Modes = append(info.Modes, m)
		}
	}
	x, y := position(cur)
	if dpi, err := d.w.dpi(x, y); err == nil && dpi > 0 {
		info.Scale = float64(dpi) / baseDPI
	}
	return info, true
}

// modeOf is a DEVMODE's size and rate. A frequency of 0 or 1 is the
// hardware's default rate, which Windows does not name.
func modeOf(dm gdi.DEVMODEW) weavewire.DisplayMode {
	m := weavewire.DisplayMode{Width: int(dm.DmPelsWidth), Height: int(dm.DmPelsHeight)}
	if dm.DmDisplayFrequency > 1 {
		m.RefreshHz = float64(dm.DmDisplayFrequency)
	}
	return m
}

// position is the display's origin on the desktop: dmPosition, the first
// member of DEVMODE's display union.
func position(dm gdi.DEVMODEW) (int32, int32) {
	p := (*[2]int32)(unsafe.Pointer(&dm.Anonymous1))
	return p[0], p[1]
}

// Errors a set can meet; DISP_CHANGE codes have no Go errors of their own.
var (
	errChange  = errors.New("ChangeDisplaySettingsEx refused the mode")
	errRestart = errors.New("the mode applies only after a restart")
	errGone    = errors.New("the display is no longer attached")
)

// Set changes a display's resolution and refresh rate.
//
// Windows has no supported API to set a monitor's scale — the per-user DPI
// setting is applied by the Settings app through undocumented DisplayConfig
// calls — so a request with a scale is refused whole, before anything
// changes, rather than applying its size and failing on the rest.
func (d displays) Set(
	ctx context.Context,
	req weavewire.DisplaySetRequest,
) (weavewire.DisplayInfo, error) {
	if req.Scale != 0 {
		return weavewire.DisplayInfo{}, &weavewire.UnsupportedError{
			Kind:   weavewire.KindDisplaySet,
			Reason: "Windows has no supported API to set a display's scale; only a resolution can be set",
		}
	}
	// The service bounds both sides by weavewire.MaxDisplayPixels.
	w, h := uint32(req.Width), uint32(req.Height) //nolint:gosec // G115: bounded, as above
	dm := gdi.DEVMODEW{
		DmFields:     gdi.DM_PELSWIDTH | gdi.DM_PELSHEIGHT,
		DmPelsWidth:  w,
		DmPelsHeight: h,
	}
	dm.DmSize = uint16(unsafe.Sizeof(dm))
	if req.RefreshHz != 0 {
		dm.DmFields |= gdi.DM_DISPLAYFREQUENCY
		dm.DmDisplayFrequency = uint32(math.Round(req.RefreshHz))
	}
	restore := d.w.aware()
	res := d.w.change(req.DisplayID, &dm, d.flags)
	restore()
	switch res {
	case gdi.DISP_CHANGE_SUCCESSFUL:
	case gdi.DISP_CHANGE_RESTART:
		return weavewire.DisplayInfo{}, fmt.Errorf(
			"%s %dx%d: %w",
			req.DisplayID,
			req.Width,
			req.Height,
			errRestart,
		)
	default:
		return weavewire.DisplayInfo{}, fmt.Errorf(
			"%w: %s %dx%d: %v",
			errChange,
			req.DisplayID,
			req.Width,
			req.Height,
			res,
		)
	}

	displays, err := d.List(ctx)
	if err != nil {
		return weavewire.DisplayInfo{}, err
	}
	for _, info := range displays {
		if info.ID == req.DisplayID {
			return info, nil
		}
	}
	return weavewire.DisplayInfo{}, fmt.Errorf("%s: %w", req.DisplayID, errGone)
}
