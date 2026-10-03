//go:build windows

package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
)

// fakeWin stands in for the clipboard itself: it holds real memory blocks
// under format numbers, counts changes and registers names, so everything
// around the clipboard — blocks, encodings, CF_HDROP parsed by the real
// DragQueryFile — runs for real without touching the clipboard of the machine
// running the test.
type fakeWin struct {
	blocks    map[uint32]foundation.HANDLE
	seq       uint32
	open      bool
	busy      int // opens to refuse before one succeeds
	regs      map[string]uint32
	noRegs    []string // names that will not register
	failEmpty bool
	failSet   uint32
	failGet   uint32
}

var errFake = errors.New("fake clipboard refused")

func newFakeWin(t *testing.T) *fakeWin {
	f := &fakeWin{blocks: map[uint32]foundation.HANDLE{}, regs: map[string]uint32{}}
	t.Cleanup(f.freeAll)
	return f
}

func (f *fakeWin) freeAll() {
	for k, h := range f.blocks {
		_, _ = foundation.GlobalFree(foundation.HGLOBAL(h))
		delete(f.blocks, k)
	}
}

func (f *fakeWin) win(t *testing.T) winClipboard {
	t.Helper()
	held := func() {
		if !f.open {
			t.Error("the clipboard was used while closed")
		}
	}
	return winClipboard{
		open: func() error {
			if f.busy > 0 {
				f.busy--
				return errFake
			}
			if f.open {
				t.Error("opened twice")
			}
			f.open = true
			return nil
		},
		close: func() error { f.open = false; return nil },
		empty: func() error {
			held()
			if f.failEmpty {
				return errFake
			}
			f.freeAll()
			f.seq++
			return nil
		},
		sequence: func() uint32 { return f.seq },
		enum: func(after uint32) uint32 {
			held()
			keys := make([]uint32, 0, len(f.blocks))
			for k := range f.blocks {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				if k > after {
					return k
				}
			}
			return 0
		},
		get: func(format uint32) (foundation.HANDLE, error) {
			held()
			h, ok := f.blocks[format]
			if !ok || format == f.failGet {
				return 0, errFake
			}
			return h, nil
		},
		set: func(format uint32, h foundation.HANDLE) error {
			held()
			if format == f.failSet {
				return errFake
			}
			f.blocks[format] = h
			f.seq++
			return nil
		},
		register: func(name string) uint32 {
			if slices.Contains(f.noRegs, name) {
				return 0
			}
			if id, ok := f.regs[name]; ok {
				return id
			}
			f.regs[name] = 0xC000 + uint32(len(f.regs))
			return f.regs[name]
		},
		dragFile: systemClipboard().dragFile,
	}
}

// put places raw bytes on the fake clipboard as format.
func (f *fakeWin) put(t *testing.T, format uint32, raw []byte) {
	t.Helper()
	h, err := newBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if old, ok := f.blocks[format]; ok {
		_, _ = foundation.GlobalFree(foundation.HGLOBAL(old))
	}
	f.blocks[format] = h
	f.seq++
}

// held reads the raw bytes the fake clipboard holds as format.
func (f *fakeWin) held(t *testing.T, format uint32) []byte {
	t.Helper()
	h, ok := f.blocks[format]
	if !ok {
		return nil
	}
	b, ok := readBlock(h)
	if !ok {
		t.Fatalf("format %d holds an unreadable block", format)
	}
	return b
}

// backend is the real backend over the fake clipboard, run on the real
// clipboard thread.
func backend(t *testing.T) (*clipboard, *fakeWin) {
	t.Helper()
	f := newFakeWin(t)
	return &clipboard{w: f.win(t), run: onClipboardThread, stage: &stager{}}, f
}
