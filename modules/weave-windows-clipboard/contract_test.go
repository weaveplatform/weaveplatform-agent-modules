//go:build windows

package main

import (
	"os"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard/weaveclipboardtest"
)

// The clipboard contract every guest OS keeps. Windows has no private
// clipboard, so the real backend runs over a stand-in for the Win32 clipboard
// calls (fake_test.go): every memory block, encoding and CF_HDROP parse is
// the real one, and the clipboard of the machine running the test is never
// written. A Windows session always has a clipboard, so there is no
// unavailable state to check.
func TestContract(t *testing.T) {
	weaveclipboardtest.RunContract(t, weaveclipboardtest.Contract{
		New: func(t *testing.T) weaveclipboard.Backend {
			c, _ := backend(t)
			return c
		},
	})
}

// The same contract against this session's real clipboard, which it
// overwrites: only on a machine that is there to be written, such as a
// throwaway VM, and only when asked for with WEAVE_CLIPBOARD_CONTRACT_SYSTEM=1.
func TestContractOnTheSystemClipboard(t *testing.T) {
	if os.Getenv("WEAVE_CLIPBOARD_CONTRACT_SYSTEM") != "1" {
		t.Skip("writes this session's clipboard; set WEAVE_CLIPBOARD_CONTRACT_SYSTEM=1 to run it")
	}
	weaveclipboardtest.RunContract(t, weaveclipboardtest.Contract{
		New: func(*testing.T) weaveclipboard.Backend { return newClipboard() },
	})
}
