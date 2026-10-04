//go:build darwin

package main

import (
	"testing"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/appkit"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard/weaveclipboardtest"
)

// The clipboard contract every guest OS keeps, against the real NSPasteboard
// backend over a uniquely named pasteboard of each check's own: every
// pasteboard call is the real one, and the clipboard of the machine running
// the test is never written. Without a pasteboard server (no GUI session)
// each check skips, saying so.
func TestContract(t *testing.T) {
	weaveclipboardtest.RunContract(t, weaveclipboardtest.Contract{
		New: func(t *testing.T) weaveclipboard.Backend { return private(t) },
		Unavailable: func(*testing.T) weaveclipboard.Backend {
			return &clipboard{board: func() *appkit.Pasteboard { return nil }, stage: &stager{}}
		},
	})
}
