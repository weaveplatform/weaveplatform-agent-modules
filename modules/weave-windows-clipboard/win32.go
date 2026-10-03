//go:build windows

package main

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/dataexchange"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/memory"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/ui/shell"
)

// winClipboard is the Win32 clipboard, one call per field, so tests can stand
// in for the clipboard itself and exercise everything around it — memory
// blocks included, which are real — without touching the clipboard of the
// machine running them.
type winClipboard struct {
	open     func() error
	close    func() error
	empty    func() error
	sequence func() uint32
	enum     func(after uint32) uint32
	get      func(format uint32) (foundation.HANDLE, error)
	set      func(format uint32, h foundation.HANDLE) error
	register func(name string) uint32
	dragFile func(h foundation.HANDLE, i uint32, buf []uint16) uint32
}

func systemClipboard() winClipboard {
	return winClipboard{
		open:     func() error { return dataexchange.OpenClipboard(0) },
		close:    dataexchange.CloseClipboard,
		empty:    dataexchange.EmptyClipboard,
		sequence: dataexchange.GetClipboardSequenceNumber,
		// The binding reports an error from the thread's last-error value
		// even when the call returns a format; the format is the answer, and
		// 0 ends the list either way.
		enum: func(after uint32) uint32 {
			f, _ := dataexchange.EnumClipboardFormats(after)
			return f
		},
		get: dataexchange.GetClipboardData,
		set: func(format uint32, h foundation.HANDLE) error {
			if _, err := dataexchange.SetClipboardData(format, h); err != nil {
				return fmt.Errorf("SetClipboardData: %w", err)
			}
			return nil
		},
		register: func(name string) uint32 {
			id, _ := dataexchange.RegisterClipboardFormat(name)
			return id
		},
		dragFile: func(h foundation.HANDLE, i uint32, buf []uint16) uint32 {
			var p *uint16
			if len(buf) > 0 {
				p = &buf[0]
			}
			n := uint32(len(buf)) //nolint:gosec // G115: a path buffer, far under 4 GiB
			return shell.DragQueryFile(shell.HDROP(h), i, p, n)
		},
	}
}

// The clipboard is owned per thread: OpenClipboard ties it to the calling
// thread, and CloseClipboard must come from the same one. A goroutine can move
// between threads at any preemption point — the retry sleep in openRetrying
// included — so every clipboard call runs on one thread locked for the life
// of the process, which also serialises them.
var (
	threadOnce sync.Once
	calls      chan func()
)

func onClipboardThread(fn func()) {
	threadOnce.Do(func() {
		calls = make(chan func())
		go func() {
			runtime.LockOSThread()
			for f := range calls {
				f()
			}
		}()
	})
	done := make(chan struct{})
	calls <- func() {
		defer close(done)
		fn()
	}
	<-done
}

// Opening is retried for a moment: OpenClipboard fails outright while another
// process holds the clipboard, and an application routinely holds it briefly
// after its own copy.
const (
	openAttempts = 20
	openBackoff  = 25 * time.Millisecond
)

var errHeld = errors.New("the clipboard is held by another process")

func (w winClipboard) openRetrying() error {
	var err error
	for range openAttempts {
		if err = w.open(); err == nil {
			return nil
		}
		time.Sleep(openBackoff)
	}
	return fmt.Errorf("%w: %w", errHeld, err)
}

// readBlock copies a clipboard memory block out. The block belongs to the
// clipboard, so it is locked for the copy and never freed here.
func readBlock(h foundation.HANDLE) ([]byte, bool) {
	hg := foundation.HGLOBAL(h)
	// GlobalSize's result is the answer; the binding's error comes from the
	// thread's last-error value, which a successful call need not clear.
	size, _ := memory.GlobalSize(hg)
	if size == 0 {
		return nil, false
	}
	p, err := memory.GlobalLock(hg)
	if err != nil {
		return nil, false
	}
	defer func() { _ = memory.GlobalUnlock(hg) }()
	return append([]byte(nil), unsafe.Slice((*byte)(p), size)...), true
}

// newBlock copies data into a movable memory block, the only kind the
// clipboard accepts. It is zeroed: an empty representation still needs a
// block of one byte, and that byte must read as a terminator, not garbage.
func newBlock(data []byte) (foundation.HANDLE, error) {
	hg, err := memory.GlobalAlloc(memory.GHND, uintptr(max(len(data), 1)))
	if err != nil {
		return 0, fmt.Errorf("GlobalAlloc %d bytes: %w", len(data), err)
	}
	p, err := memory.GlobalLock(hg)
	if err != nil {
		_, _ = foundation.GlobalFree(hg)
		return 0, fmt.Errorf("GlobalLock: %w", err)
	}
	copy(unsafe.Slice((*byte)(p), len(data)), data)
	_ = memory.GlobalUnlock(hg) // 0 once unlocked, which the binding reports as an error
	return foundation.HANDLE(hg), nil
}

// writeFormat hands data to the clipboard as format. The block becomes the
// clipboard's once the set succeeds, so it is freed only when it fails;
// freeing it afterwards would corrupt the clipboard.
func (w winClipboard) writeFormat(format uint32, data []byte) error {
	h, err := newBlock(data)
	if err != nil {
		return err
	}
	if err := w.set(format, h); err != nil {
		_, _ = foundation.GlobalFree(foundation.HGLOBAL(h))
		return fmt.Errorf("format %d: %w", format, err)
	}
	return nil
}
