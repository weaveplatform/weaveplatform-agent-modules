//go:build darwin

package main

import (
	"context"
	"errors"
	"strconv"
	"testing"

	cg "github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/coregraphics"
	"github.com/ebitengine/purego/objc"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// realDisplay returns the first active display of the machine running the
// test, skipping when there is none (a headless runner).
func realDisplay(t *testing.T) uint32 {
	t.Helper()
	ids, err := activeDisplays()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Skip("no active display")
	}
	return ids[0]
}

// Against the real window server: read only.
func TestListReadsTheRealDisplays(t *testing.T) {
	realDisplay(t)
	got, err := newDisplays().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("an active display was not listed")
	}
	primaries := 0
	for _, d := range got {
		if d.Primary {
			primaries++
		}
		if d.Current.Width <= 0 || d.Current.Height <= 0 || d.Scale < 1 || len(d.Modes) == 0 {
			t.Errorf("display %+v", d)
		}
		// The current mode is one a set can choose again.
		id, _ := strconv.ParseUint(d.ID, 10, 32)
		cur, ok := currentMode(uint32(id))
		if !ok {
			t.Fatalf("display %s has no current mode", d.ID)
		}
		if _, err := choose(
			allModes(uint32(id)),
			cur,
			weavewire.DisplaySetRequest{Scale: d.Scale},
		); err != nil {
			t.Errorf("display %s: its own current mode is not selectable: %v", d.ID, err)
		}
	}
	if primaries != 1 {
		t.Errorf("%d primary displays", primaries)
	}
	if _, ok := currentMode(0xFFFFFFF0); ok {
		t.Error("a display that does not exist has a mode")
	}
}

// The real transaction calls, never completed: complete is replaced by a
// cancel, so the machine running the test keeps its display mode whatever
// CoreGraphics makes of the request.
func TestApplyRunsTheRealTransactionWithoutCompletingIt(t *testing.T) {
	id := realDisplay(t)
	cur, _ := currentMode(id)
	cfg := coreGraphicsConfig()
	completed := false
	cfg.complete = func(c cg.CGDisplayConfigRef) cg.CGError {
		completed = true
		return cfg.cancel(c)
	}
	if err := cfg.apply(id, cur); err != nil {
		t.Fatalf("configuring the current mode: %v", err)
	}
	if !completed {
		t.Error("the transaction was never completed")
	}
}

func TestApplyReportsEachFailure(t *testing.T) {
	id := realDisplay(t)
	cur, _ := currentMode(id)
	fail := cg.CGError(1001) // kCGErrorFailure
	ok := func(cg.CGDisplayConfigRef) cg.CGError { return cg.KCGErrorSuccess }
	cancelled := false
	for name, c := range map[string]cgConfig{
		"begin": {begin: func(*objc.ID) cg.CGError { return fail }},
		"configure": {
			begin:     func(*objc.ID) cg.CGError { return cg.KCGErrorSuccess },
			configure: func(cg.CGDisplayConfigRef, uint32, cg.CGDisplayModeRef) cg.CGError { return fail },
			cancel:    func(cg.CGDisplayConfigRef) cg.CGError { cancelled = true; return cg.KCGErrorSuccess },
		},
		"complete": {
			begin:     func(*objc.ID) cg.CGError { return cg.KCGErrorSuccess },
			configure: func(cg.CGDisplayConfigRef, uint32, cg.CGDisplayModeRef) cg.CGError { return cg.KCGErrorSuccess },
			complete:  func(cg.CGDisplayConfigRef) cg.CGError { return fail },
			cancel:    ok,
		},
	} {
		if err := c.apply(id, cur); err == nil {
			t.Errorf("%s failure was not reported", name)
		}
	}
	if !cancelled {
		t.Error("a failed configure was not cancelled")
	}
	if err := coreGraphicsConfig().apply(id, mode{}); !errors.Is(err, errNoMode) {
		t.Errorf("a mode with no handle: err = %v", err)
	}
}
