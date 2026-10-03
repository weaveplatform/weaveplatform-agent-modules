//go:build darwin

package main

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/corefoundation"
	cg "github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/coregraphics"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/obj"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/purego"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/rt"
	"github.com/ebitengine/purego/objc"
)

// errCoreGraphics carries a CGError, which has no sentinel of its own.
var errCoreGraphics = errors.New("CoreGraphics refused")

// maxDisplays bounds CGGetActiveDisplayList's buffer. A Mac drives a handful
// of displays at most; a VM one or two.
const maxDisplays = 32

// coreGraphics is the quartz surface over the house binding, pure Go on
// purego, so the module stays CGO_ENABLED=0.
func coreGraphics() quartz {
	return quartz{
		active:  activeDisplays,
		isMain:  func(id uint32) bool { return cg.CGDisplayIsMain(id) != 0 },
		current: currentMode,
		modes:   allModes,
		apply:   coreGraphicsConfig().apply,
	}
}

func coreGraphicsConfig() cgConfig {
	return cgConfig{
		begin: func(ref *objc.ID) cg.CGError { return cg.CGBeginDisplayConfiguration(unsafe.Pointer(ref)) },
		configure: func(c cg.CGDisplayConfigRef, id uint32, m cg.CGDisplayModeRef) cg.CGError {
			return cg.CGConfigureDisplayWithDisplayMode(c, id, m, nil)
		},
		complete: func(c cg.CGDisplayConfigRef) cg.CGError {
			// For the login session, not permanently: the host asks again
			// on every resize of its window, and a mode it chose should not
			// become the user's saved preference for this display.
			return cg.CGCompleteDisplayConfiguration(c, cg.KCGConfigureForSession)
		},
		cancel: cg.CGCancelDisplayConfiguration,
	}
}

func activeDisplays() ([]uint32, error) {
	ids := make([]uint32, maxDisplays)
	res, n := cg.CGGetActiveDisplayList(maxDisplays, unsafe.Pointer(&ids[0]))
	if res != cg.KCGErrorSuccess {
		return nil, fmt.Errorf(
			"%w: CGGetActiveDisplayList: %v",
			errCoreGraphics,
			res,
		)
	}
	return ids[:min(n, maxDisplays)], nil
}

// The binding adopts what CGDisplayCopyDisplayMode and
// CGDisplayCopyAllDisplayModes return (+1 references) and releases them when
// Go drops them, so CGDisplayModeRelease is never called here: it would
// release them a second time.

func currentMode(id uint32) (mode, bool) {
	ref := cg.CGDisplayCopyDisplayMode(id)
	if ref.IsNil() {
		return mode{}, false
	}
	return describe(ref), true
}

func describe(ref cg.CGDisplayModeRef) mode {
	return mode{
		width:   cg.CGDisplayModeGetWidth(ref),
		height:  cg.CGDisplayModeGetHeight(ref),
		pixelW:  cg.CGDisplayModeGetPixelWidth(ref),
		pixelH:  cg.CGDisplayModeGetPixelHeight(ref),
		refresh: cg.CGDisplayModeGetRefreshRate(ref),
		usable:  cg.CGDisplayModeIsUsableForDesktopGUI(ref),
		handle:  ref,
	}
}

// modeOptions asks for the HiDPI modes too. Without
// kCGDisplayShowDuplicateLowResolutionModes CoreGraphics lists each pixel
// size once, at 1x, and a Retina mode could be neither listed nor set.
//
// It is built once: the dictionary is autoreleased, and a module thread has
// no pool to drain it, so building one per list would leak one per list.
var modeOptions = sync.OnceValue(func() obj.Object {
	key := obj.ID(cg.KCGDisplayShowDuplicateLowResolutionModes())
	val := obj.ID(corefoundation.KCFBooleanTrue())
	self := func(id objc.ID) objc.ID { return id }
	return obj.Wrap(rt.MapToDict(map[objc.ID]objc.ID{key: val}, self, self))
})

func allModes(id uint32) []mode {
	arr := cg.CGDisplayCopyAllDisplayModes(id, modeOptions())
	refs := purego.NSArrayToSlice(obj.ID(arr), func(m objc.ID) cg.CGDisplayModeRef {
		return cg.CGDisplayModeRef{Object: obj.Wrap(m)}
	})
	out := make([]mode, 0, len(refs))
	for _, r := range refs {
		out = append(out, describe(r))
	}
	return out
}

// cgConfig is a display configuration transaction. Its steps are fields so a
// test runs the real begin, configure and cancel without ever completing a
// change on the machine running it.
type cgConfig struct {
	begin     func(ref *objc.ID) cg.CGError
	configure func(c cg.CGDisplayConfigRef, id uint32, m cg.CGDisplayModeRef) cg.CGError
	complete  func(c cg.CGDisplayConfigRef) cg.CGError
	cancel    func(c cg.CGDisplayConfigRef) cg.CGError
}

func (c cgConfig) apply(id uint32, m mode) error {
	ref, ok := m.handle.(cg.CGDisplayModeRef)
	if !ok || ref.IsNil() {
		return fmt.Errorf("display %d: %w", id, errNoMode)
	}
	var raw objc.ID
	if res := c.begin(&raw); res != cg.KCGErrorSuccess {
		return fmt.Errorf(
			"%w: CGBeginDisplayConfiguration: %v",
			errCoreGraphics,
			res,
		)
	}
	// A configuration is a plain handle, not a CF object: Complete or Cancel
	// frees it, so it carries no retain or release of its own.
	config := cg.CGDisplayConfigRef{Object: obj.WrapUnmanaged(raw)}
	if res := c.configure(config, id, ref); res != cg.KCGErrorSuccess {
		_ = c.cancel(config)
		return fmt.Errorf(
			"%w: CGConfigureDisplayWithDisplayMode %d: %v",
			errCoreGraphics,
			id,
			res,
		)
	}
	if res := c.complete(config); res != cg.KCGErrorSuccess {
		return fmt.Errorf("%w: CGCompleteDisplayConfiguration: %v", errCoreGraphics, res)
	}
	return nil
}
