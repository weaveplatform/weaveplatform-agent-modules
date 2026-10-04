//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"os"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// clipboard is the console user's clipboard, through whichever command-line
// tool speaks to the session's display server. Linux has no clipboard API
// below the display server, and linking a Wayland or X11 client library would
// take the module off pure Go; the tools are what every desktop ships.
//
// core starts the module inside the console session with that session's
// WAYLAND_DISPLAY or DISPLAY, so the environment says which server to talk to.
type clipboard struct {
	tool *tool
	// missing says why there is no tool, for the unsupported answer.
	missing string
	stage   *stager
	// sets counts this module's own writes. It is part of the token, so a set
	// changes the token even when it writes what the clipboard already held.
	sets atomic.Uint64
}

func newClipboard() *clipboard {
	return detect(os.Getenv, lookPath)
}

// detect picks the tool for the session's display server: wl-clipboard on
// Wayland, xclip on X11. A Wayland session without wl-clipboard but with
// XWayland and xclip still has a working clipboard through X11, which
// Wayland applications share.
func detect(getenv func(string) string, look func(string) bool) *clipboard {
	c := &clipboard{stage: &stager{}}
	wayland, x11 := getenv("WAYLAND_DISPLAY"), getenv("DISPLAY")
	switch {
	case wayland != "" && look("wl-paste") && look("wl-copy"):
		c.tool = wlClipboard()
	case x11 != "" && look("xclip"):
		c.tool = xclip()
	case wayland != "":
		c.missing = fmt.Sprintf("the session's Wayland display %q needs wl-clipboard "+
			"(wl-paste and wl-copy), which is not installed", wayland)
	case x11 != "":
		c.missing = fmt.Sprintf("the session's X11 display %q needs xclip, "+
			"which is not installed", x11)
	default:
		c.missing = "the session has no Wayland or X11 display " +
			"(WAYLAND_DISPLAY and DISPLAY are both unset)"
	}
	return c
}

// Support reports every canonical format, each under the target it is
// written as, and that the tools hold one of them per set.
func (c *clipboard) Support() weaveclipboard.Support {
	natives := make(map[weavewire.ClipboardFormat]string)
	for f := range targetsFor {
		if c.tool != nil {
			natives[f] = c.tool.writeTarget(f)
		} else {
			natives[f] = targetsFor[f][0]
		}
	}
	s := weaveclipboard.CanonicalSupport(natives)
	s.SingleRepresentation = true
	s.Limitation = "the clipboard is written through a command-line tool, which holds one " +
		"representation per copy: a set keeps the richest"
	return s
}

func (c *clipboard) unsupported(kind string) error {
	if c.tool != nil {
		return nil
	}
	return &weavewire.UnsupportedError{Kind: kind, Reason: c.missing}
}

// representation is one format the clipboard offers: the target it is read
// from and, once read, its bytes.
type representation struct {
	format weavewire.ClipboardFormat
	target string
}

// offered maps the clipboard's targets to the formats they carry, in
// weavewire's richest-first order.
func offered(targets []string) []representation {
	var out []representation
	for _, f := range weavewire.ClipboardFormats() {
		if t, ok := pickTarget(targets, f); ok {
			out = append(out, representation{format: f, target: t})
		}
	}
	return out
}

// snapshot is one read of every mapped representation, and its token.
type snapshot struct {
	token uint64
	reps  []content
}

type content struct {
	format weavewire.ClipboardFormat
	data   []byte
}

// snapshot reads the clipboard.
//
// Linux has no change counter — neither X11 selections nor the Wayland
// data-device protocol number their changes — so the token is a digest of
// the content itself: the target list and every mapped representation's
// bytes. That reads the clipboard on every stat, which costs a process per
// format and the bytes of an image when one is copied; a digest of the
// targets alone would be cheap, but would miss a second image copied over the
// first, which offers exactly the same targets. A sync loop that never sees a
// change is worse than one that reads a few hundred kilobytes a second.
func (c *clipboard) snapshot(ctx context.Context) (snapshot, error) {
	targets, err := c.tool.targets(ctx)
	if err != nil {
		return snapshot{}, err
	}
	h := fnv.New64a()
	_, _ = h.Write(binary.LittleEndian.AppendUint64(nil, c.sets.Load()))
	for _, t := range targets {
		_, _ = h.Write([]byte(t))
		_, _ = h.Write([]byte{0})
	}
	var s snapshot
	for _, r := range offered(targets) {
		data, err := c.tool.paste(ctx, r.target)
		if err != nil {
			// The owner may have changed between the list and the read;
			// leave the format out rather than fail.
			continue
		}
		_, _ = h.Write([]byte(r.format))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		s.reps = append(s.reps, content{format: r.format, data: data})
	}
	s.token = h.Sum64()
	return s, nil
}

// Stat reports the change token and the formats on offer, with their sizes:
// the digest has read them anyway.
func (c *clipboard) Stat(ctx context.Context) (weavewire.ClipboardStatResponse, error) {
	if err := c.unsupported(weavewire.KindClipboardStat); err != nil {
		return weavewire.ClipboardStatResponse{}, err
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return weavewire.ClipboardStatResponse{}, err
	}
	resp := weavewire.ClipboardStatResponse{ChangeToken: s.token}
	for _, r := range s.reps {
		info := weavewire.ClipboardFormatInfo{Format: r.format, Size: int64(len(r.data))}
		if r.format == weavewire.ClipboardFiles {
			info.Size = 0
			for _, p := range parseURIList(r.data) {
				if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
					info.Size += fi.Size()
					info.Count++
				}
			}
			if info.Count == 0 {
				continue // a list of directories or vanished paths is nothing to copy
			}
		}
		resp.Formats = append(resp.Formats, info)
	}
	return resp, nil
}

// Read returns the representations asked for, and the token of exactly what
// it read.
func (c *clipboard) Read(
	ctx context.Context,
	formats []weavewire.ClipboardFormat,
	maxBytes int64,
) (weaveclipboard.Contents, error) {
	if err := c.unsupported(weavewire.KindClipboardGet); err != nil {
		return weaveclipboard.Contents{}, err
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return weaveclipboard.Contents{}, err
	}
	out := weaveclipboard.Contents{ChangeToken: s.token}
	for _, r := range s.reps {
		switch {
		case len(formats) > 0 && !slices.Contains(formats, r.format):
		case r.format == weavewire.ClipboardFiles:
			out.Items = append(out.Items, readFiles(parseURIList(r.data), maxBytes)...)
		default:
			out.Items = append(out.Items, weavewire.ClipboardItem{
				Format: r.format, Size: int64(len(r.data)), Data: r.data,
			})
		}
	}
	return out, nil
}

// Write replaces the clipboard with the richest representation it was given.
//
// wl-copy and xclip each offer one target per copy — a second invocation
// replaces the first rather than adding to it — so the others are dropped,
// and Written says which one the clipboard now holds.
func (c *clipboard) Write(
	ctx context.Context,
	items []weavewire.ClipboardItem,
) (weavewire.ClipboardSetResponse, error) {
	if err := c.unsupported(weavewire.KindClipboardSet); err != nil {
		return weavewire.ClipboardSetResponse{}, err
	}
	format, ok := richest(items)
	if !ok {
		// Nothing a Linux clipboard can hold: the OS accepted nothing, and
		// the clipboard is as it was.
		s, err := c.snapshot(ctx)
		return weavewire.ClipboardSetResponse{ChangeToken: s.token}, err
	}
	var data []byte
	if format == weavewire.ClipboardFiles {
		paths, err := c.stage.files(items)
		if err != nil {
			return weavewire.ClipboardSetResponse{}, err
		}
		data = uriList(paths)
	} else {
		for _, it := range items {
			if it.Format == format {
				data = it.Data
				break
			}
		}
	}
	if err := c.tool.copy(ctx, c.tool.writeTarget(format), data); err != nil {
		return weavewire.ClipboardSetResponse{}, err
	}
	c.sets.Add(1)
	s, err := c.snapshot(ctx)
	if err != nil {
		return weavewire.ClipboardSetResponse{}, err
	}
	return weavewire.ClipboardSetResponse{
		ChangeToken: s.token,
		Written:     []weavewire.ClipboardFormat{format},
	}, nil
}

// richest picks the format to write: the first of weavewire's richest-first
// list that the items carry and a Linux clipboard can hold.
func richest(items []weavewire.ClipboardItem) (weavewire.ClipboardFormat, bool) {
	for _, f := range weavewire.ClipboardFormats() {
		if _, known := targetsFor[f]; !known {
			continue
		}
		if slices.ContainsFunc(items, func(it weavewire.ClipboardItem) bool {
			return it.Format == f
		}) {
			return f, true
		}
	}
	return "", false
}

// readFiles reads each copied file's content: the paths mean nothing on the
// host, so the content is what crosses. A directory or unreadable path is
// skipped, and a file over maxBytes is returned with its size and no data so
// the service lists it as omitted rather than truncating it.
func readFiles(paths []string, maxBytes int64) []weavewire.ClipboardItem {
	var out []weavewire.ClipboardItem
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		it := weavewire.ClipboardItem{
			Format: weavewire.ClipboardFiles,
			Name:   baseName(p),
			Size:   fi.Size(),
		}
		if fi.Size() <= maxBytes {
			data, err := os.ReadFile(p) //nolint:gosec // G304: the path is one the user copied
			if err != nil {
				continue
			}
			it.Data, it.Size = data, int64(len(data))
		}
		out = append(out, it)
	}
	return out
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
