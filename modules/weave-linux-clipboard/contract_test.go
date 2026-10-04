//go:build linux

package main

import (
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard/weaveclipboardtest"
)

// The clipboard contract every guest OS keeps. Here the real backend drives
// wl-clipboard stand-ins (fake_test.go), which keep their clipboard in a
// directory of the test's own and, like wl-copy, hold one representation per
// copy; with no display at all every op is unsupported.
func TestContract(t *testing.T) {
	weaveclipboardtest.RunContract(t, weaveclipboardtest.Contract{
		New: func(t *testing.T) weaveclipboard.Backend {
			installFake(t)
			return wayland(t)
		},
		Unavailable: func(*testing.T) weaveclipboard.Backend {
			return detect(func(string) string { return "" }, lookPath)
		},
	})
}
