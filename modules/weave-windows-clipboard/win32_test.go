//go:build windows

package main

import (
	"errors"
	"testing"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/memory"
)

func TestReadBlockOfNothing(t *testing.T) {
	if b, ok := readBlock(0); ok || b != nil {
		t.Errorf("read %q, %v from no block", b, ok)
	}
}

func TestReadBlockThatWillNotLock(t *testing.T) {
	h, err := newBlock([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = foundation.GlobalFree(foundation.HGLOBAL(h)) })
	swap(t, &globalLock, func(foundation.HGLOBAL) (unsafe.Pointer, error) { return nil, errInjected })
	if _, ok := readBlock(h); ok {
		t.Error("read a block that would not lock")
	}
}

func TestNewBlockFailures(t *testing.T) {
	t.Run("alloc", func(t *testing.T) {
		swap(t, &globalAlloc, func(memory.GLOBAL_ALLOC_FLAGS, uintptr) (foundation.HGLOBAL, error) {
			return 0, errInjected
		})
		if _, err := newBlock([]byte("x")); !errors.Is(err, errInjected) {
			t.Errorf("newBlock with no memory: %v", err)
		}
	})
	t.Run("lock", func(t *testing.T) {
		swap(t, &globalLock, func(foundation.HGLOBAL) (unsafe.Pointer, error) { return nil, errInjected })
		if _, err := newBlock([]byte("x")); !errors.Is(err, errInjected) {
			t.Errorf("newBlock that would not lock: %v", err)
		}
	})
}

func TestWriteFormatWithNoBlock(t *testing.T) {
	swap(t, &globalAlloc, func(memory.GLOBAL_ALLOC_FLAGS, uintptr) (foundation.HGLOBAL, error) {
		return 0, errInjected
	})
	w := winClipboard{set: func(uint32, foundation.HANDLE) error {
		t.Error("set reached with no block")
		return nil
	}}
	if err := w.writeFormat(1, []byte("x")); !errors.Is(err, errInjected) {
		t.Errorf("writeFormat with no memory: %v", err)
	}
}

// The real SetClipboardData, given format 0, which no clipboard accepts: it
// fails without the clipboard open, so the runner's clipboard is untouched.
func TestSystemSetReportsItsFailure(t *testing.T) {
	h, err := newBlock([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = foundation.GlobalFree(foundation.HGLOBAL(h)) })
	if err := systemClipboard().set(0, h); err == nil {
		t.Error("SetClipboardData accepted format 0")
	}
}
