//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"slices"
	"sync"
	"syscall"
	"unicode/utf16"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Standard clipboard formats, from WinUser.h.
const (
	cfTIFF        = 6
	cfUnicodeText = 13
	cfHDrop       = 15
)

// Registered format names: the ones Windows applications agree on for HTML,
// RTF and PNG, which have no standard format number, and the name PDF is held
// under.
//
// No name for PDF is shared the way these are, so PDF is held under a name
// registered for it, "Portable Document Format", which any application that
// registers the same name shares: it round-trips host to guest to host, which
// keeps a PDF from being dropped on its way through a Windows guest, and stat
// reports it as private, since most Windows applications neither paste nor
// copy it.
var registeredNames = map[weavewire.ClipboardFormat]string{
	weavewire.ClipboardHTML: "HTML Format",
	weavewire.ClipboardRTF:  "Rich Text Format",
	weavewire.ClipboardPNG:  "PNG",
	weavewire.ClipboardPDF:  "Portable Document Format",
}

// clipboard is the console user's clipboard through the Win32 API. core
// starts the module in the user's session on its interactive desktop
// (winsta0\default); a service in session 0 has a clipboard of its own that
// no user ever sees.
type clipboard struct {
	w     winClipboard
	run   func(func())
	stage *stager

	mu         sync.Mutex
	registered map[weavewire.ClipboardFormat]uint32
}

func newClipboard() *clipboard {
	return &clipboard{w: systemClipboard(), run: onClipboardThread, stage: &stager{}}
}

// natives are the clipboard format names each weave format is held under.
var natives = map[weavewire.ClipboardFormat]string{
	weavewire.ClipboardText:  "CF_UNICODETEXT",
	weavewire.ClipboardHTML:  registeredNames[weavewire.ClipboardHTML],
	weavewire.ClipboardRTF:   registeredNames[weavewire.ClipboardRTF],
	weavewire.ClipboardPNG:   registeredNames[weavewire.ClipboardPNG],
	weavewire.ClipboardTIFF:  "CF_TIFF",
	weavewire.ClipboardPDF:   registeredNames[weavewire.ClipboardPDF],
	weavewire.ClipboardFiles: "CF_HDROP",
}

// Support reports every canonical format held at once, under its format name,
// and PDF's name as private to weave.
func (c *clipboard) Support() weaveclipboard.Support {
	s := weaveclipboard.CanonicalSupport(natives)
	for i, f := range s.Formats {
		if f.Format == weavewire.ClipboardPDF {
			s.Formats[i].Private = true
		}
	}
	return s
}

// formatID is the clipboard format a weave format is held as, 0 for none. A
// registered format's number is assigned per session at run time, and stays
// fixed for the session once registered.
func (c *clipboard) formatID(f weavewire.ClipboardFormat) uint32 {
	switch f {
	case weavewire.ClipboardText:
		return cfUnicodeText
	case weavewire.ClipboardTIFF:
		return cfTIFF
	case weavewire.ClipboardFiles:
		return cfHDrop
	}
	name, ok := registeredNames[f]
	if !ok {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if id, ok := c.registered[f]; ok {
		return id
	}
	id := c.w.register(name)
	if id != 0 {
		if c.registered == nil {
			c.registered = make(map[weavewire.ClipboardFormat]uint32)
		}
		c.registered[f] = id
	}
	return id
}

// offer is what the open clipboard holds: the weave formats, richest first,
// and the bitmap formats in the order the clipboard lists them.
type offer struct {
	formats []weavewire.ClipboardFormat
	// bitmaps are CF_DIBV5 and CF_DIB as they are listed: an application's
	// own formats come before those Windows synthesizes from them, so the
	// first is the one the application wrote.
	bitmaps []uint32
	png     bool // PNG itself, under its registered name
}

// available reads what the open clipboard holds. A bitmap with no PNG
// beside it, as Paint copies, is offered as PNG: converted when it is read.
// CF_BITMAP alone counts, since Windows synthesizes CF_DIB from it for any
// reader.
func (c *clipboard) available() offer {
	var o offer
	held := make(map[uint32]bool)
	for f := c.w.enum(0); f != 0; f = c.w.enum(f) {
		held[f] = true
		if f == cfDIBV5 || f == cfDIB {
			o.bitmaps = append(o.bitmaps, f)
		}
	}
	if held[cfBitmap] && !held[cfDIB] {
		o.bitmaps = append(o.bitmaps, cfDIB)
	}
	for _, f := range weavewire.ClipboardFormats() {
		id := c.formatID(f)
		has := id != 0 && held[id]
		if f == weavewire.ClipboardPNG {
			o.png = has
			has = has || len(o.bitmaps) > 0
		}
		if has {
			o.formats = append(o.formats, f)
		}
	}
	return o
}

// files lists the paths in the open clipboard's CF_HDROP.
func (c *clipboard) files() []string {
	h, err := c.w.get(cfHDrop)
	if err != nil {
		return nil
	}
	n := c.w.dragFile(h, ^uint32(0), nil)
	paths := make([]string, 0, n)
	for i := range n {
		buf := make([]uint16, c.w.dragFile(h, i, nil)+1)
		if c.w.dragFile(h, i, buf) == 0 {
			continue
		}
		paths = append(paths, syscall.UTF16ToString(buf))
	}
	return paths
}

// withOpen runs fn with the clipboard open, on the clipboard thread.
func (c *clipboard) withOpen(fn func() error) error {
	var err error
	c.run(func() {
		if err = c.w.openRetrying(); err != nil {
			return
		}
		defer func() { _ = c.w.close() }()
		err = fn()
	})
	return err
}

// Stat reports the clipboard's sequence number and the formats it holds.
// The sequence number advances on every change by any process and needs no
// window or listener, which is exactly a change token.
//
// Sizes are left out except for files, whose sizes the filesystem knows: an
// application may hold its data unrendered until asked, and asking for it
// would make the application render it. weaveclipboard's service sizes the
// rest by reading them, once per change of the sequence number rather than on
// every poll.
func (c *clipboard) Stat(context.Context) (weavewire.ClipboardStatResponse, error) {
	var resp weavewire.ClipboardStatResponse
	err := c.withOpen(func() error {
		resp.ChangeToken = uint64(c.w.sequence())
		for _, f := range c.available().formats {
			info := weavewire.ClipboardFormatInfo{Format: f}
			if f == weavewire.ClipboardFiles {
				for _, p := range c.files() {
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

// Read returns the representations asked for. Nothing can change the
// clipboard while it is open, so the read is consistent with the sequence
// number read alongside it. A representation is handed over whole, so the
// host's cap is the service's to apply; files are offered by path.
func (c *clipboard) Read(
	_ context.Context,
	formats []weavewire.ClipboardFormat,
	_ int64,
) (weaveclipboard.Contents, error) {
	var out weaveclipboard.Contents
	err := c.withOpen(func() error {
		out.ChangeToken = uint64(c.w.sequence())
		o := c.available()
		for _, f := range o.formats {
			if len(formats) > 0 && !slices.Contains(formats, f) {
				continue
			}
			if f == weavewire.ClipboardFiles {
				// By path, unread: the service streams them from disk.
				out.Files = weaveclipboard.FilesAt(c.files())
				continue
			}
			if f == weavewire.ClipboardPNG && !o.png {
				if data := c.bitmapPNG(o.bitmaps); data != nil {
					out.Items = append(out.Items, weavewire.ClipboardItem{
						Format: f, Size: int64(len(data)), Data: data,
					})
				}
				continue
			}
			h, err := c.w.get(c.formatID(f))
			if err != nil {
				continue // the owner failed to render it; leave it out
			}
			raw, ok := readBlock(h)
			if !ok {
				continue
			}
			if data := decode(f, raw); len(data) > 0 {
				out.Items = append(out.Items, weavewire.ClipboardItem{
					Format: f, Size: int64(len(data)), Data: data,
				})
			}
		}
		return nil
	})
	return out, err
}

// Write empties the clipboard and puts every representation it can hold on
// it. Files are staged and offered as CF_HDROP, the shape Explorer copies, so
// a paste anywhere copies real files.
func (c *clipboard) Write(
	ctx context.Context,
	items []weavewire.ClipboardItem,
) (weavewire.ClipboardSetResponse, error) {
	var paths []string
	if slices.ContainsFunc(items, func(it weavewire.ClipboardItem) bool {
		return it.Format == weavewire.ClipboardFiles
	}) {
		var err error
		if paths, err = c.stage.files(items); err != nil {
			return weavewire.ClipboardSetResponse{}, err
		}
	}
	return c.WriteFiles(ctx, items, paths)
}

// WriteFiles is Write with the files already staged, at paths: the service
// streams a host's files to disk and hands them over here by path.
func (c *clipboard) WriteFiles(
	_ context.Context,
	items []weavewire.ClipboardItem,
	paths []string,
) (weavewire.ClipboardSetResponse, error) {
	var resp weavewire.ClipboardSetResponse
	err := c.withOpen(func() error {
		if err := c.w.empty(); err != nil {
			return fmt.Errorf("EmptyClipboard: %w", err)
		}
		for _, it := range items {
			id := c.formatID(it.Format)
			if id == 0 || slices.Contains(resp.Written, it.Format) {
				continue
			}
			data := encode(it.Format, it.Data)
			if it.Format == weavewire.ClipboardFiles {
				data = dropFiles(paths)
			}
			if err := c.w.writeFormat(id, data); err != nil {
				return err
			}
			resp.Written = append(resp.Written, it.Format)
			if it.Format == weavewire.ClipboardPNG {
				c.writeBitmaps(it.Data)
			}
		}
		return nil
	})
	if err != nil {
		return weavewire.ClipboardSetResponse{}, err
	}
	// Read after closing: the sequence number counts the change once the
	// clipboard is released.
	c.run(func() { resp.ChangeToken = uint64(c.w.sequence()) })
	return resp, nil
}

// bitmapPNG converts the first bitmap on the open clipboard that converts to
// PNG, nil when none does.
func (c *clipboard) bitmapPNG(formats []uint32) []byte {
	for _, f := range formats {
		h, err := c.w.get(f)
		if err != nil {
			continue
		}
		raw, ok := readBlock(h)
		if !ok {
			continue
		}
		if data, err := dibToPNG(raw); err == nil {
			return data
		}
	}
	return nil
}

// writeBitmaps puts a PNG on the open clipboard as CF_DIBV5 and CF_DIB as
// well, so applications that paste only a bitmap, Paint among them, paste
// it. They are extra renderings of the PNG already written: one that cannot
// be made (a PNG that does not decode, or is too large) or written leaves
// the PNG on the clipboard as it is, rather than failing a set that wrote
// what it was asked to.
func (c *clipboard) writeBitmaps(data []byte) {
	dib, dibv5, err := pngToDIBs(data)
	if err != nil {
		return
	}
	_ = c.w.writeFormat(cfDIBV5, dibv5)
	_ = c.w.writeFormat(cfDIB, dib)
}

// decode turns a clipboard block into the weave format's bytes.
func decode(f weavewire.ClipboardFormat, raw []byte) []byte {
	switch f {
	case weavewire.ClipboardText:
		return []byte(decodeUTF16(raw))
	case weavewire.ClipboardHTML:
		return []byte(fromCFHTML(raw))
	case weavewire.ClipboardRTF:
		return trimNUL(raw)
	default:
		return raw
	}
}

// encode turns a weave format's bytes into the block the clipboard holds.
func encode(f weavewire.ClipboardFormat, data []byte) []byte {
	switch f {
	case weavewire.ClipboardText:
		return encodeUTF16(string(data))
	case weavewire.ClipboardHTML:
		return toCFHTML(string(data))
	case weavewire.ClipboardRTF:
		return append(trimNUL(data), 0)
	default:
		return data
	}
}

// decodeUTF16 reads CF_UNICODETEXT: UTF-16LE up to its NUL. The block may be
// longer than the text in it.
func decodeUTF16(raw []byte) string {
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return syscall.UTF16ToString(units)
}

// encodeUTF16 writes CF_UNICODETEXT: UTF-16LE with its NUL.
func encodeUTF16(s string) []byte {
	units := append(utf16.Encode([]rune(s)), 0)
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// trimNUL cuts a NUL-terminated block at its terminator.
func trimNUL(b []byte) []byte {
	if i := slices.Index(b, 0); i >= 0 {
		return b[:i]
	}
	return b
}
