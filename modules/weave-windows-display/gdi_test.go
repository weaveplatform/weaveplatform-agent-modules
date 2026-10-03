//go:build windows

package main

import (
	"context"
	"errors"
	"testing"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/graphics/gdi"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

func utf16(s string, n int) []uint16 {
	out := make([]uint16, n)
	for i, r := range s {
		out[i] = uint16(r)
	}
	return out
}

func device(name, desc string, flags gdi.DISPLAY_DEVICE_STATE_FLAGS) gdi.DISPLAY_DEVICEW {
	var dd gdi.DISPLAY_DEVICEW
	copy(dd.DeviceName[:], utf16(name, len(dd.DeviceName)))
	copy(dd.DeviceString[:], utf16(desc, len(dd.DeviceString)))
	dd.StateFlags = flags
	return dd
}

func devmode(w, h, hz uint32, x, y int32) gdi.DEVMODEW {
	dm := gdi.DEVMODEW{DmPelsWidth: w, DmPelsHeight: h, DmDisplayFrequency: hz}
	p := (*[2]int32)(unsafe.Pointer(&dm.Anonymous1))
	p[0], p[1] = x, y
	return dm
}

// fakeUser32 stands in for User32: two attached adapters and a detached one,
// and a change that records instead of applying anything.
type fakeUser32 struct {
	changed []gdi.DEVMODEW
	flags   []gdi.CDS_TYPE
	result  gdi.DISP_CHANGE
	aware   int
	gone    bool
}

func (f *fakeUser32) win32() win32 {
	adapters := []gdi.DISPLAY_DEVICEW{
		device(
			`\\.\DISPLAY1`,
			"Microsoft Hyper-V Video",
			gdi.DISPLAY_DEVICE_ATTACHED_TO_DESKTOP|gdi.DISPLAY_DEVICE_PRIMARY_DEVICE,
		),
		device(`\\.\DISPLAY2`, "Microsoft Hyper-V Video", 0),
		device(`\\.\DISPLAY3`, "Basic Display", gdi.DISPLAY_DEVICE_ATTACHED_TO_DESKTOP),
		device(`\\.\DISPLAY4`, "Vanishing", gdi.DISPLAY_DEVICE_ATTACHED_TO_DESKTOP),
	}
	modes := map[string][]gdi.DEVMODEW{
		`\\.\DISPLAY1`: {
			devmode(1920, 1080, 60, 0, 0),
			devmode(1920, 1080, 60, 0, 0),
			devmode(1280, 720, 1, 0, 0),
		},
		`\\.\DISPLAY3`: {devmode(1024, 768, 75, 1920, 0)},
	}
	return win32{
		device: func(parent *string, i uint32) (gdi.DISPLAY_DEVICEW, bool) {
			if parent != nil {
				if *parent == `\\.\DISPLAY1` && i == 0 {
					return device(`\\.\DISPLAY1\Monitor0`, "Generic PnP Monitor", 0), true
				}
				return gdi.DISPLAY_DEVICEW{}, false
			}
			if int(i) >= len(adapters) {
				return gdi.DISPLAY_DEVICEW{}, false
			}
			return adapters[i], true
		},
		settings: func(name string, i gdi.ENUM_DISPLAY_SETTINGS_MODE) (gdi.DEVMODEW, bool) {
			list := modes[name]
			if name == `\\.\DISPLAY1` && f.gone {
				return gdi.DEVMODEW{}, false
			}
			if i == gdi.ENUM_CURRENT_SETTINGS {
				if len(list) == 0 {
					return gdi.DEVMODEW{}, false
				}
				return list[0], true
			}
			if int(i) >= len(list) {
				return gdi.DEVMODEW{}, false
			}
			return list[i], true
		},
		dpi: func(x, _ int32) (uint32, error) {
			if x == 0 {
				return 144, nil
			}
			return 0, errors.New("no monitor there")
		},
		change: func(_ string, dm *gdi.DEVMODEW, flags gdi.CDS_TYPE) gdi.DISP_CHANGE {
			f.changed = append(f.changed, *dm)
			f.flags = append(f.flags, flags)
			return f.result
		},
		aware: func() func() { f.aware++; return func() { f.aware-- } },
	}
}

func TestListReportsAttachedDisplays(t *testing.T) {
	f := &fakeUser32{}
	got, err := displays{w: f.win32()}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %+v, want DISPLAY1 and DISPLAY3", got)
	}
	main := got[0]
	if main.ID != `\\.\DISPLAY1` || main.Name != "Generic PnP Monitor" || !main.Primary ||
		main.Scale != 1.5 ||
		main.Current != (weavewire.DisplayMode{Width: 1920, Height: 1080, RefreshHz: 60}) {
		t.Errorf("primary = %+v", main)
	}
	// The duplicate (another colour depth) is one mode; rate 1 is "default".
	want := []weavewire.DisplayMode{
		{Width: 1920, Height: 1080, RefreshHz: 60},
		{Width: 1280, Height: 720},
	}
	if len(main.Modes) != 2 || main.Modes[0] != want[0] || main.Modes[1] != want[1] {
		t.Errorf("modes = %v, want %v", main.Modes, want)
	}
	if got[1].Name != "Basic Display" || got[1].Primary || got[1].Scale != 0 {
		t.Errorf("second = %+v", got[1])
	}
	if f.aware != 0 {
		t.Error("the thread's DPI awareness was not restored")
	}
}

func TestSetChangesTheModeDynamically(t *testing.T) {
	f := &fakeUser32{}
	d := displays{w: f.win32()}
	got, err := d.Set(context.Background(), weavewire.DisplaySetRequest{
		DisplayID: `\\.\DISPLAY1`, Width: 1280, Height: 720, RefreshHz: 59.94,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != `\\.\DISPLAY1` || len(f.changed) != 1 || f.flags[0] != 0 {
		t.Fatalf("reported %+v, changed %+v with %v", got, f.changed, f.flags)
	}
	dm := f.changed[0]
	if dm.DmPelsWidth != 1280 || dm.DmPelsHeight != 720 || dm.DmDisplayFrequency != 60 ||
		dm.DmFields != gdi.DM_PELSWIDTH|gdi.DM_PELSHEIGHT|gdi.DM_DISPLAYFREQUENCY ||
		int(dm.DmSize) != int(unsafe.Sizeof(dm)) {
		t.Errorf("DEVMODE %+v", dm)
	}

	if _, err := d.Set(context.Background(), weavewire.DisplaySetRequest{
		DisplayID: `\\.\DISPLAY3`, Width: 800, Height: 600,
	}); err != nil || f.changed[1].DmFields&gdi.DM_DISPLAYFREQUENCY != 0 {
		t.Errorf("a set with no rate: %v, %+v", err, f.changed[1])
	}
}

func TestSetFailures(t *testing.T) {
	ctx := context.Background()
	f := &fakeUser32{}
	d := displays{w: f.win32()}

	_, err := d.Set(
		ctx,
		weavewire.DisplaySetRequest{DisplayID: `\\.\DISPLAY1`, Width: 800, Height: 600, Scale: 2},
	)
	if _, ok := errors.AsType[*weavewire.UnsupportedError](err); !ok || len(f.changed) != 0 {
		t.Errorf("scale: err = %v, changed %d", err, len(f.changed))
	}

	req := weavewire.DisplaySetRequest{DisplayID: `\\.\DISPLAY1`, Width: 800, Height: 600}
	f.result = gdi.DISP_CHANGE_RESTART
	if _, err := d.Set(ctx, req); !errors.Is(err, errRestart) {
		t.Errorf("restart: err = %v", err)
	}
	f.result = gdi.DISP_CHANGE_BADMODE
	if _, err := d.Set(ctx, req); !errors.Is(err, errChange) {
		t.Errorf("bad mode: err = %v", err)
	}
	f.result, f.gone = gdi.DISP_CHANGE_SUCCESSFUL, true
	if _, err := d.Set(ctx, req); !errors.Is(err, errGone) {
		t.Errorf("display gone: err = %v", err)
	}
}
