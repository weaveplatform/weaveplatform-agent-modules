//go:build darwin

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// A Retina panel as CoreGraphics lists it: each pixel size at 2x and 1x.
var (
	retina2880 = mode{
		width:   1440,
		height:  900,
		pixelW:  2880,
		pixelH:  1800,
		refresh: 60,
		usable:  true,
	}
	plain2880 = mode{
		width:   2880,
		height:  1800,
		pixelW:  2880,
		pixelH:  1800,
		refresh: 60,
		usable:  true,
	}
	retina2048 = mode{
		width:   1024,
		height:  640,
		pixelW:  2048,
		pixelH:  1280,
		refresh: 60,
		usable:  true,
	}
	fast2048 = mode{
		width:   1024,
		height:  640,
		pixelW:  2048,
		pixelH:  1280,
		refresh: 120,
		usable:  true,
	}
	unusable   = mode{width: 640, height: 480, pixelW: 640, pixelH: 480, refresh: 60}
	panelModes = []mode{retina2880, plain2880, retina2048, fast2048, unusable}
)

// fakeQuartz stands in for CoreGraphics: two displays, the second of which
// may vanish, and an apply that records instead of reconfiguring anything.
type fakeQuartz struct {
	cur      map[uint32]mode
	applied  []mode
	applyErr error
	listErr  error
}

func (f *fakeQuartz) quartz() quartz {
	return quartz{
		active: func() ([]uint32, error) {
			if f.listErr != nil {
				return nil, f.listErr
			}
			return []uint32{1, 2, 9}, nil
		},
		isMain: func(id uint32) bool { return id == 1 },
		current: func(id uint32) (mode, bool) {
			m, ok := f.cur[id]
			return m, ok
		},
		modes: func(uint32) []mode { return panelModes },
		apply: func(id uint32, m mode) error {
			if f.applyErr != nil {
				return f.applyErr
			}
			f.applied = append(f.applied, m)
			f.cur[id] = m
			return nil
		},
	}
}

func newFake() *fakeQuartz {
	return &fakeQuartz{cur: map[uint32]mode{1: retina2880, 2: plain2880}}
}

func TestListReportsPixelsScaleAndUsableModes(t *testing.T) {
	f := newFake()
	got, err := displays{q: f.quartz()}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d displays, want 2 (display 9 has no current mode)", len(got))
	}
	main := got[0]
	if main.ID != "1" || !main.Primary || main.Scale != 2 ||
		main.Current != (weavewire.DisplayMode{Width: 2880, Height: 1800, RefreshHz: 60}) {
		t.Errorf("main display = %+v", main)
	}
	// 2880x1800 at 2x and at 1x is one pixel mode; 640x480 is not usable.
	want := []weavewire.DisplayMode{
		{Width: 2880, Height: 1800, RefreshHz: 60},
		{Width: 2048, Height: 1280, RefreshHz: 60},
		{Width: 2048, Height: 1280, RefreshHz: 120},
	}
	if len(main.Modes) != len(want) {
		t.Fatalf("modes = %v, want %v", main.Modes, want)
	}
	for i := range want {
		if main.Modes[i] != want[i] {
			t.Errorf("mode %d = %v, want %v", i, main.Modes[i], want[i])
		}
	}
	if got[1].Primary || got[1].Scale != 1 {
		t.Errorf("second display = %+v", got[1])
	}

	f.listErr = errors.New("no window server")
	if _, err := (displays{q: f.quartz()}).List(context.Background()); !errors.Is(err, f.listErr) {
		t.Errorf("err = %v", err)
	}
}

func TestChoose(t *testing.T) {
	for name, tc := range map[string]struct {
		cur  mode
		req  weavewire.DisplaySetRequest
		want mode
	}{
		"size keeps the current scale":   {retina2880, weavewire.DisplaySetRequest{Width: 2048, Height: 1280}, retina2048},
		"size keeps the current refresh": {mode{width: 1, pixelW: 1, refresh: 120}, weavewire.DisplaySetRequest{Width: 2048, Height: 1280}, fast2048},
		"scale alone keeps the size":     {retina2880, weavewire.DisplaySetRequest{Scale: 1}, plain2880},
		"refresh picks among a size":     {retina2880, weavewire.DisplaySetRequest{Width: 2048, Height: 1280, RefreshHz: 120}, fast2048},
		"scale and size together":        {plain2880, weavewire.DisplaySetRequest{Width: 2880, Height: 1800, Scale: 2}, retina2880},
	} {
		got, err := choose(panelModes, tc.cur, tc.req)
		if err != nil || got != tc.want {
			t.Errorf("%s: chose %+v (%v), want %+v", name, got, err, tc.want)
		}
	}
	for name, req := range map[string]weavewire.DisplaySetRequest{
		"unlisted size":    {Width: 1920, Height: 1080},
		"unlisted scale":   {Scale: 3},
		"unlisted refresh": {Width: 2880, Height: 1800, RefreshHz: 144},
		"unusable mode":    {Width: 640, Height: 480},
	} {
		if _, err := choose(panelModes, retina2880, req); !errors.Is(err, errNoMode) {
			t.Errorf("%s: err = %v, want errNoMode", name, err)
		}
	}
}

func TestSetAppliesTheChosenModeAndReportsTheResult(t *testing.T) {
	f := newFake()
	d := displays{q: f.quartz()}
	got, err := d.Set(
		context.Background(),
		weavewire.DisplaySetRequest{DisplayID: "1", Width: 2048, Height: 1280},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.applied) != 1 || f.applied[0] != retina2048 {
		t.Errorf("applied %+v", f.applied)
	}
	if got.Current.Width != 2048 || got.Scale != 2 {
		t.Errorf("reported %+v", got)
	}
}

func TestSetReportsEachFailure(t *testing.T) {
	f := newFake()
	d := displays{q: f.quartz()}
	ctx := context.Background()
	if _, err := d.Set(ctx, weavewire.DisplaySetRequest{DisplayID: "main", Scale: 1}); err == nil {
		t.Error("a non-numeric display id was accepted")
	}
	if _, err := d.Set(
		ctx,
		weavewire.DisplaySetRequest{DisplayID: "9", Scale: 1},
	); !errors.Is(
		err,
		errGone,
	) {
		t.Errorf("vanished display: err = %v", err)
	}
	if _, err := d.Set(
		ctx,
		weavewire.DisplaySetRequest{DisplayID: "1", Width: 7, Height: 7},
	); !errors.Is(
		err,
		errNoMode,
	) {
		t.Errorf("unlisted mode: err = %v", err)
	}
	f.applyErr = errors.New("refused")
	if _, err := d.Set(
		ctx,
		weavewire.DisplaySetRequest{DisplayID: "1", Scale: 1},
	); !errors.Is(
		err,
		f.applyErr,
	) {
		t.Errorf("apply failure: err = %v", err)
	}

	// The display disappears between the change and the read-back.
	f.applyErr = nil
	q := f.quartz()
	q.apply = func(id uint32, _ mode) error { delete(f.cur, id); return nil }
	if _, err := (displays{q: q}).Set(
		ctx,
		weavewire.DisplaySetRequest{DisplayID: "2", Scale: 2},
	); !errors.Is(
		err,
		errGone,
	) {
		t.Errorf("display gone after the change: err = %v", err)
	}
}

func TestModeScale(t *testing.T) {
	if s := (mode{}).scale(); s != 0 {
		t.Errorf("scale of an empty mode = %v", s)
	}
}
