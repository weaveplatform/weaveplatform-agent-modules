//go:build windows

package main

import (
	"runtime"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/graphics/gdi"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/ui/hidpi"
)

// user32 is the win32 surface over the house binding.
func user32() win32 {
	return win32{
		device:   enumDevice,
		settings: enumSettings,
		dpi:      monitorDPI,
		change: func(name string, dm *gdi.DEVMODEW, flags gdi.CDS_TYPE) gdi.DISP_CHANGE {
			return gdi.ChangeDisplaySettingsEx(&name, dm, flags, nil)
		},
		aware: perMonitorAware,
	}
}

func enumDevice(parent *string, i uint32) (gdi.DISPLAY_DEVICEW, bool) {
	dd := gdi.DISPLAY_DEVICEW{}
	dd.Cb = uint32(unsafe.Sizeof(dd))
	return dd, gdi.EnumDisplayDevices(parent, i, &dd, 0)
}

func enumSettings(name string, i gdi.ENUM_DISPLAY_SETTINGS_MODE) (gdi.DEVMODEW, bool) {
	dm := gdi.DEVMODEW{}
	dm.DmSize = uint16(unsafe.Sizeof(dm))
	return dm, gdi.EnumDisplaySettings(&name, i, &dm)
}

func monitorDPI(x, y int32) (uint32, error) {
	mon := gdi.MonitorFromPoint(foundation.POINT{X: x, Y: y}, gdi.MONITOR_DEFAULTTONULL)
	var dpiX, dpiY uint32
	if err := hidpi.GetDpiForMonitor(mon, hidpi.MDT_EFFECTIVE_DPI, &dpiX, &dpiY); err != nil {
		return 0, err //nolint:wrapcheck // the caller treats any failure as "no scale"
	}
	return dpiX, nil
}

// perMonitorAware makes this thread per-monitor DPI aware for the calls that
// follow, and returns the restore.
//
// A process that never declared DPI awareness is shown a virtualised desktop:
// GetDpiForMonitor answers 96 for every monitor and display modes can be
// scaled to match. Awareness is per thread here, not per process, so nothing
// else in the module changes; the thread is locked for as long as it holds it.
func perMonitorAware() func() {
	runtime.LockOSThread()
	prev := hidpi.SetThreadDpiAwarenessContext(hidpi.DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2)
	return func() {
		if prev != 0 {
			hidpi.SetThreadDpiAwarenessContext(prev)
		}
		runtime.UnlockOSThread()
	}
}
