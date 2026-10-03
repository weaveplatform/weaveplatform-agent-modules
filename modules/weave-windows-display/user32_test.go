//go:build windows

package main

import (
	"context"
	"testing"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/graphics/gdi"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// realDisplays lists the displays of the machine running the test, skipping
// when it has none (a service session, a headless runner).
func realDisplays(t *testing.T) []weavewire.DisplayInfo {
	t.Helper()
	got, err := newGDI().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Skip("no display attached to this desktop")
	}
	return got
}

// Against the real User32: read only.
func TestListReadsTheRealDisplays(t *testing.T) {
	for _, d := range realDisplays(t) {
		if d.ID == "" || d.Current.Width <= 0 || d.Current.Height <= 0 || len(d.Modes) == 0 {
			t.Errorf("display %+v", d)
		}
		t.Logf(
			"%s %q primary=%v %v scale %v, %d modes",
			d.ID,
			d.Name,
			d.Primary,
			d.Current,
			d.Scale,
			len(d.Modes),
		)
	}
	if _, err := monitorDPI(-1<<30, -1<<30); err == nil {
		t.Error("a point on no monitor has a DPI")
	}
}

// The real ChangeDisplaySettingsEx with CDS_TEST, which validates the mode
// without applying it: the display of the machine running the test keeps
// its mode.
func TestSetValidatesTheCurrentModeWithoutApplyingIt(t *testing.T) {
	d := realDisplays(t)[0]
	g := displays{w: user32(), flags: gdi.CDS_TEST}
	got, err := g.Set(context.Background(), weavewire.DisplaySetRequest{
		DisplayID: d.ID,
		Width:     d.Current.Width,
		Height:    d.Current.Height,
		RefreshHz: d.Current.RefreshHz,
	})
	if err != nil {
		t.Fatalf("testing the current mode: %v", err)
	}
	if got.Current != d.Current {
		t.Errorf("a tested mode changed the display: %v, was %v", got.Current, d.Current)
	}
	if _, err := g.Set(context.Background(), weavewire.DisplaySetRequest{
		DisplayID: d.ID, Width: 16384, Height: 16384,
	}); err == nil {
		t.Error("a 16K mode passed the test")
	}
}
