//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/appkit"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/foundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/obj"
	"github.com/ebitengine/purego/objc"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// clipboard is the console user's general pasteboard, through NSPasteboard
// in the house binding — pure Go on purego, so the module stays CGO_ENABLED=0.
// core starts the module inside the user's GUI session (gui/<uid>), which is
// the bootstrap the pasteboard server is reached through; as a daemon it would
// see no pasteboard at all.
//
// The pasteboard is a field so tests run against a uniquely named pasteboard
// of their own and never write to the clipboard of the machine running them.
type clipboard struct {
	board func() *appkit.Pasteboard

	mu    sync.Mutex
	stage *stager
}

func newClipboard() *clipboard {
	return &clipboard{board: appkit.GeneralPasteboard, stage: &stager{}}
}

// utis maps each format to the uniform type identifier it is held under.
var utis = map[weavewire.ClipboardFormat]string{
	weavewire.ClipboardText:  "public.utf8-plain-text",
	weavewire.ClipboardHTML:  "public.html",
	weavewire.ClipboardRTF:   "public.rtf",
	weavewire.ClipboardPNG:   "public.png",
	weavewire.ClipboardTIFF:  "public.tiff",
	weavewire.ClipboardPDF:   "com.adobe.pdf",
	weavewire.ClipboardFiles: "public.file-url",
}

var (
	loadOnce  sync.Once
	poolClass objc.ID
	selNew    = objc.RegisterName("new")
	selDrain  = objc.RegisterName("drain")
)

// do runs fn against the pasteboard inside an autorelease pool, one call at a
// time.
//
// Every AppKit call returns autoreleased objects. A Go thread has no pool of
// its own to drain them, so without one each stat — which a host makes every
// second or so — would leak a little for the life of the session. The pool
// is per thread, so the goroutine is locked to its thread until it drains.
//
// The pool comes from the Objective-C runtime directly rather than the
// binding's NSAutoreleasePool wrapper: that wrapper releases its object from a
// finalizer, and -drain has already released and freed it, so the finalizer
// would release freed memory.
func (c *clipboard) do(fn func(pb *appkit.Pasteboard) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	loadOnce.Do(func() {
		// The pool class lives in Foundation, which the binding loads on
		// first use; asking it for a constant loads it.
		_ = appkit.NSPasteboardTypeString()
		poolClass = objc.ID(objc.GetClass("NSAutoreleasePool"))
	})
	pool := poolClass.Send(selNew)
	defer pool.Send(selDrain)
	return fn(c.board())
}

func uti(f weavewire.ClipboardFormat) obj.Object {
	return foundation.NewStringWithString(utis[f])
}

// offered lists the formats the pasteboard holds, richest first.
func offered(pb *appkit.Pasteboard) []weavewire.ClipboardFormat {
	types := make(map[string]bool)
	for _, t := range pb.Types() {
		types[t.Description()] = true
	}
	var out []weavewire.ClipboardFormat
	for _, f := range weavewire.ClipboardFormats() {
		if types[utis[f]] {
			out = append(out, f)
		}
	}
	return out
}

// filePaths lists the local files on the pasteboard, one per item.
func filePaths(pb *appkit.Pasteboard) []string {
	var paths []string
	for _, it := range pb.PasteboardItems() {
		if p, ok := fileURLPath(it.StringForType(uti(weavewire.ClipboardFiles))); ok {
			paths = append(paths, p)
		}
	}
	return paths
}

// fileURLPath turns a pasteboard file URL into a path. Finder puts file
// reference URLs there (file:///.file/id=…), which name a file by its inode
// rather than its path; Foundation resolves one to the path it has now (the
// binding hands a file URL back as its path).
func fileURLPath(s string) (string, bool) {
	if strings.HasPrefix(s, "file:///.file/") {
		p := foundation.URLWithString(s).FilePathURL()
		return p, strings.HasPrefix(p, "/")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return "", false
	}
	return u.Path, true
}

// Stat reports the pasteboard's change count and the formats it holds.
//
// Sizes are left out except for files, whose sizes the filesystem knows: the
// pasteboard holds most data only as a promise from the application that
// copied it, and asking for its size would make that application render it.
func (c *clipboard) Stat(context.Context) (weavewire.ClipboardStatResponse, error) {
	var resp weavewire.ClipboardStatResponse
	err := c.do(func(pb *appkit.Pasteboard) error {
		resp.ChangeToken = token(pb.ChangeCount())
		for _, f := range offered(pb) {
			info := weavewire.ClipboardFormatInfo{Format: f}
			if f == weavewire.ClipboardFiles {
				for _, p := range filePaths(pb) {
					if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
						info.Size += fi.Size()
						info.Count++
					}
				}
				if info.Count == 0 {
					continue // folders, or files gone since: nothing to copy
				}
			}
			resp.Formats = append(resp.Formats, info)
		}
		return nil
	})
	return resp, err
}

// token is the change count as the wire carries it. NSPasteboard's
// changeCount is an NSInteger that only ever counts up from zero.
func token(count int) uint64 {
	return uint64(count) //nolint:gosec // G115: never negative
}

// readAttempts bounds the reads that may be overtaken by a new copy before the
// last one is returned anyway. Its token is the count from before it read, so
// a host that got a mix of two copies sees the count move on its next stat and
// reads again.
const readAttempts = 3

// Read returns the representations asked for, and the change count they were
// read at.
func (c *clipboard) Read(
	_ context.Context,
	formats []weavewire.ClipboardFormat,
	maxBytes int64,
) (weaveclipboard.Contents, error) {
	var out weaveclipboard.Contents
	err := c.do(func(pb *appkit.Pasteboard) error {
		for range readAttempts {
			before := pb.ChangeCount()
			out = weaveclipboard.Contents{ChangeToken: token(before)}
			for _, f := range offered(pb) {
				switch {
				case len(formats) > 0 && !slices.Contains(formats, f):
				case f == weavewire.ClipboardFiles:
					out.Items = append(out.Items, readFiles(filePaths(pb), maxBytes)...)
				default:
					if data := pb.DataForType(uti(f)); data != nil {
						out.Items = append(out.Items, weavewire.ClipboardItem{
							Format: f, Size: int64(len(data)), Data: data,
						})
					}
				}
			}
			if pb.ChangeCount() == before {
				break
			}
		}
		return nil
	})
	return out, err
}

var errRefused = errors.New("the pasteboard refused the write")

// Write replaces the pasteboard's content. Every representation goes onto one
// pasteboard item, as an application's own copy does; files are staged and
// written as items of their own, one per file, which is what Finder pastes.
func (c *clipboard) Write(
	_ context.Context,
	items []weavewire.ClipboardItem,
) (weavewire.ClipboardSetResponse, error) {
	var resp weavewire.ClipboardSetResponse
	var paths []string
	if slices.ContainsFunc(items, func(it weavewire.ClipboardItem) bool {
		return it.Format == weavewire.ClipboardFiles
	}) {
		var err error
		if paths, err = c.stage.files(items); err != nil {
			return resp, err
		}
	}
	err := c.do(func(pb *appkit.Pasteboard) error {
		pb.ClearContents()
		if len(paths) > 0 {
			objects := make([]obj.Object, 0, len(paths))
			for _, p := range paths {
				it := appkit.NewPasteboardItem()
				it.SetStringForType(
					(&url.URL{Scheme: "file", Path: p}).String(),
					uti(weavewire.ClipboardFiles),
				)
				objects = append(objects, it)
			}
			if !pb.WriteObjects(objects) {
				return fmt.Errorf("%w: %d files", errRefused, len(paths))
			}
			resp.Written = append(resp.Written, weavewire.ClipboardFiles)
		}
		for _, it := range items {
			if _, known := utis[it.Format]; !known || it.Format == weavewire.ClipboardFiles ||
				slices.Contains(resp.Written, it.Format) {
				continue
			}
			if pb.SetDataForType(it.Data, uti(it.Format)) {
				resp.Written = append(resp.Written, it.Format)
			}
		}
		resp.ChangeToken = token(pb.ChangeCount())
		return nil
	})
	return resp, err
}
