//go:build linux

package main

import (
	"os"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard/weaveclipboardtest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// The clipboard contract every guest OS keeps, run against each way the
// module reaches a Linux clipboard, each over a clipboard of the test's own:
//
//   - data control, ext and wlr, against an in-process compositor
//     (wlfake_test.go);
//   - a real compositor with data control, when one was started for the
//     tests and named by WEAVE_CLIPBOARD_TEST_WAYLAND_DISPLAY (a headless Sway
//     in `make test-linux-clipboard`);
//   - X11 against a display started for the tests and named by
//     WEAVE_CLIPBOARD_TEST_DISPLAY (Xvfb in CI);
//   - the wl-clipboard fallback against stand-ins for wl-paste and wl-copy
//     (fake_test.go), which, like wl-copy, hold one representation per copy;
//
// and, with no display at all, every op answers unsupported.
func TestContract(t *testing.T) {
	unavailable := func(*testing.T) weaveclipboard.Backend { return testClipboard(map[string]string{}) }
	t.Run("ext-data-control", func(t *testing.T) {
		weaveclipboardtest.RunContract(t, weaveclipboardtest.Contract{
			New: func(t *testing.T) weaveclipboard.Backend {
				return dataControlClipboard(t, newFakeCompositor(t, true, false))
			},
			Unavailable: unavailable,
		})
	})
	t.Run("wlr-data-control", func(t *testing.T) {
		weaveclipboardtest.RunContract(t, weaveclipboardtest.Contract{
			New: func(t *testing.T) weaveclipboard.Backend {
				return dataControlClipboard(t, newFakeCompositor(t, false, true))
			},
		})
	})
	t.Run("compositor", func(t *testing.T) {
		display := os.Getenv("WEAVE_CLIPBOARD_TEST_WAYLAND_DISPLAY")
		if display == "" {
			t.Skip(
				"no compositor for the tests: set WEAVE_CLIPBOARD_TEST_WAYLAND_DISPLAY to one started for them",
			)
		}
		weaveclipboardtest.RunContract(t, weaveclipboardtest.Contract{
			New: func(t *testing.T) weaveclipboard.Backend {
				c := testClipboard(map[string]string{
					"WAYLAND_DISPLAY": display, "XDG_RUNTIME_DIR": os.Getenv("XDG_RUNTIME_DIR"),
				})
				c.dialWayland = newClipboard().dialWayland
				t.Cleanup(c.closeMechanism)
				if _, err := c.mechanism(weavewire.KindClipboardStat); err != nil {
					t.Fatal(err)
				}
				return c
			},
		})
	})
	t.Run("x11", func(t *testing.T) {
		testDisplay(t)
		weaveclipboardtest.RunContract(t, weaveclipboardtest.Contract{
			New: func(t *testing.T) weaveclipboard.Backend { return x11Clipboard(t) },
		})
	})
	t.Run("wl-clipboard", func(t *testing.T) {
		weaveclipboardtest.RunContract(t, weaveclipboardtest.Contract{
			New: func(t *testing.T) weaveclipboard.Backend {
				installFake(t)
				return toolClipboard(t)
			},
		})
	})
}
