//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// clipboard is the console user's clipboard, through the session's display
// server. core starts the module inside the console session with that
// session's WAYLAND_DISPLAY or DISPLAY, so the environment says which server
// to talk to, and the module talks to it in pure Go:
//
//   - Wayland, through a data-control protocol (wayland.go) where the
//     compositor has one;
//   - X11 — a plain X session, or XWayland under a compositor without data
//     control (GNOME) — as the CLIPBOARD selection's owner (x11.go);
//   - failing both, wl-clipboard's wl-paste and wl-copy (tool.go), which hold
//     one representation per copy. Stat says so.
//
// The first two hold every representation of a set at once, as the macOS
// pasteboard and the Windows clipboard do. The connection is made at the
// first op and made again after it breaks (a compositor restart).
type clipboard struct {
	getenv      func(string) string
	look        func(string) bool
	dialWayland func(path string) (mechanism, error)
	dialX11     func(display string) (mechanism, error)

	mu    sync.Mutex
	m     mechanism
	stage *stager
}

// mechanism is one way of reaching the clipboard.
type mechanism interface {
	// name says what the clipboard is reached through, for a limitation.
	name() string
	// single reports a mechanism that holds one representation per set.
	single() bool
	// offered lists the targets (X11 atoms or MIME types) the clipboard
	// offers now, and the change token.
	offered(ctx context.Context) ([]string, uint64, error)
	// read returns one target's data.
	read(ctx context.Context, target string) ([]byte, error)
	// own makes offers the clipboard and returns the token afterwards.
	own(ctx context.Context, offers []offer) (uint64, error)
	// broken reports a connection that has ended and must be made again.
	broken() bool
	close()
}

// offer is one target a set offers, with its data.
type offer struct {
	target string
	data   []byte
}

// maxRead caps one representation read from the clipboard, which is held in
// memory: one larger is reported sized and unread (errTooLarge), so the
// service lists it as omitted rather than truncating it. A variable so the
// tests can reach it. Files are not read here: they stream from disk.
var maxRead int64 = weavewire.MaxClipboardRepresentationBytes

// errTooLarge reports a representation over maxRead.
var errTooLarge = errors.New("the representation is larger than one read may hold")

func newClipboard() *clipboard {
	return &clipboard{
		getenv: os.Getenv,
		look:   lookPath,
		dialWayland: func(path string) (mechanism, error) {
			d, err := dialDataControl(path)
			if err != nil {
				return nil, err
			}
			return d, nil
		},
		dialX11: func(display string) (mechanism, error) {
			x, err := dialX11(display)
			if err != nil {
				return nil, err
			}
			return x, nil
		},
		stage: &stager{},
	}
}

// mechanism returns the session's clipboard mechanism, connecting if there is
// none or the last one broke. With none to be had the op is unsupported, and
// says why, so a host can tell a session it cannot sync from a broken module.
func (c *clipboard) mechanism(kind string) (mechanism, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m != nil && !c.m.broken() {
		return c.m, nil
	}
	if c.m != nil {
		c.m.close()
		c.m = nil
	}
	m, reason := c.detect()
	if m == nil {
		return nil, &weavewire.UnsupportedError{Kind: kind, Reason: reason}
	}
	c.m = m
	return m, nil
}

// detect connects to the session's display server, richest mechanism first.
func (c *clipboard) detect() (mechanism, string) {
	wayland, display := c.getenv("WAYLAND_DISPLAY"), c.getenv("DISPLAY")
	if wayland == "" && display == "" {
		return nil, "the session has no Wayland or X11 display " +
			"(WAYLAND_DISPLAY and DISPLAY are both unset)"
	}
	var reasons []string
	if wayland != "" {
		path, err := wlSocket(wayland, c.getenv("XDG_RUNTIME_DIR"))
		if err == nil {
			var m mechanism
			if m, err = c.dialWayland(path); err == nil {
				return m, ""
			}
		}
		reasons = append(reasons, err.Error())
	}
	if display != "" {
		m, err := c.dialX11(display)
		if err == nil {
			return m, ""
		}
		reasons = append(reasons, err.Error())
	}
	if wayland != "" {
		if c.look("wl-paste") && c.look("wl-copy") {
			return wlClipboard(), ""
		}
		reasons = append(reasons, "wl-clipboard (wl-paste and wl-copy) is not installed")
	}
	return nil, strings.Join(reasons, "; ")
}

// drop forgets a mechanism whose op failed because its connection ended, so
// the next op connects again.
func (c *clipboard) drop(m mechanism) {
	if !m.broken() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == m {
		c.m.close()
		c.m = nil
	}
}

// Support reports every canonical format, each under the target it is
// offered as first, and, through wl-copy, that one is held per set.
func (c *clipboard) Support() weaveclipboard.Support {
	natives := make(map[weavewire.ClipboardFormat]string)
	for f, targets := range writeTargets {
		natives[f] = targets[0]
	}
	s := weaveclipboard.CanonicalSupport(natives)
	c.mu.Lock()
	m := c.m
	c.mu.Unlock()
	if m != nil && m.single() {
		s.SingleRepresentation = true
		s.Limitation = "the compositor offers neither ext-data-control-v1 nor " +
			"wlr-data-control-unstable-v1 and the session has no X display, so the clipboard is " +
			"written through " + m.name() + ", which holds one representation per copy: " +
			"a set keeps the richest"
	}
	return s
}

// representation is one format the clipboard offers and the target it is
// read from.
type representation struct {
	format weavewire.ClipboardFormat
	target string
}

// formatsOffered maps the clipboard's targets to the formats they carry, in
// weavewire's richest-first order.
func formatsOffered(targets []string) []representation {
	var out []representation
	for _, f := range weavewire.ClipboardFormats() {
		if t, ok := pickTarget(targets, f); ok {
			out = append(out, representation{format: f, target: t})
		}
	}
	return out
}

// Stat reports the change token and the formats on offer. Sizes are left out
// except for files, whose sizes the filesystem knows: asking the owner for its
// data would make it render every representation on every poll.
// weaveclipboard's service sizes the rest by reading them, once per change of
// the token.
func (c *clipboard) Stat(ctx context.Context) (weavewire.ClipboardStatResponse, error) {
	m, err := c.mechanism(weavewire.KindClipboardStat)
	if err != nil {
		return weavewire.ClipboardStatResponse{}, err
	}
	targets, tok, err := m.offered(ctx)
	if err != nil {
		c.drop(m)
		return weavewire.ClipboardStatResponse{}, err
	}
	resp := weavewire.ClipboardStatResponse{ChangeToken: tok}
	for _, r := range formatsOffered(targets) {
		info := weavewire.ClipboardFormatInfo{Format: r.format}
		if r.format == weavewire.ClipboardFiles {
			list, err := m.read(ctx, r.target)
			if err != nil {
				continue
			}
			for _, p := range parseURIList(list) {
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

// Read returns the representations asked for, and the token of the
// clipboard they were read from. An owner hands a representation over whole,
// so the host's cap is the service's to apply; files are offered by path.
func (c *clipboard) Read(
	ctx context.Context,
	formats []weavewire.ClipboardFormat,
	_ int64,
) (weaveclipboard.Contents, error) {
	m, err := c.mechanism(weavewire.KindClipboardGet)
	if err != nil {
		return weaveclipboard.Contents{}, err
	}
	targets, tok, err := m.offered(ctx)
	if err != nil {
		c.drop(m)
		return weaveclipboard.Contents{}, err
	}
	out := weaveclipboard.Contents{ChangeToken: tok}
	for _, r := range formatsOffered(targets) {
		if len(formats) > 0 && !slices.Contains(formats, r.format) {
			continue
		}
		data, err := m.read(ctx, r.target)
		if errors.Is(err, errTooLarge) {
			// Sized and unread: the service lists it as omitted.
			out.Items = append(
				out.Items,
				weavewire.ClipboardItem{Format: r.format, Size: maxRead + 1},
			)
			continue
		}
		if err != nil {
			// The owner may have changed between the list and the read;
			// leave the format out rather than fail.
			continue
		}
		if r.format == weavewire.ClipboardFiles {
			// By path, unread: the service streams them from disk.
			out.Files = weaveclipboard.FilesAt(parseURIList(data))
			continue
		}
		out.Items = append(out.Items, weavewire.ClipboardItem{
			Format: r.format, Size: int64(len(data)), Data: data,
		})
	}
	return out, nil
}

// Write replaces the clipboard with every representation it was given — or,
// through wl-copy, the richest — and reports which it holds. The service
// hands it only canonical formats.
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
	ctx context.Context,
	items []weavewire.ClipboardItem,
	paths []string,
) (weavewire.ClipboardSetResponse, error) {
	m, err := c.mechanism(weavewire.KindClipboardSet)
	if err != nil {
		return weavewire.ClipboardSetResponse{}, err
	}
	var formats []weavewire.ClipboardFormat
	for _, f := range weavewire.ClipboardFormats() {
		if slices.ContainsFunc(
			items,
			func(it weavewire.ClipboardItem) bool { return it.Format == f },
		) {
			formats = append(formats, f)
		}
	}
	if len(formats) == 0 {
		// The service passes only formats the clipboard holds; a caller
		// that skipped it gets the answer the service would have given.
		return weavewire.ClipboardSetResponse{}, &weavewire.UnsupportedError{
			Kind:   weavewire.KindClipboardSet,
			Reason: "none of the formats is one a clipboard holds",
		}
	}
	if m.single() {
		formats = formats[:1] // richest first
	}

	var offers []offer
	for _, f := range formats {
		if f == weavewire.ClipboardFiles {
			offers = append(offers, fileOffers(paths)...)
			continue
		}
		i := slices.IndexFunc(
			items,
			func(it weavewire.ClipboardItem) bool { return it.Format == f },
		)
		for _, t := range writeTargets[f] {
			offers = append(offers, offer{target: t, data: items[i].Data})
		}
	}
	if m.single() {
		offers = offers[:1] // wl-copy offers one target
	}
	tok, err := m.own(ctx, offers)
	if err != nil {
		c.drop(m)
		return weavewire.ClipboardSetResponse{}, err
	}
	return weavewire.ClipboardSetResponse{ChangeToken: tok, Written: formats}, nil
}

// fileOffers are the targets a copy of files is offered as: the URI list
// every file manager reads, and GNOME's own form of it, which Nautilus pastes
// as a copy rather than a link.
func fileOffers(paths []string) []offer {
	gnome := "copy"
	for _, p := range paths {
		gnome += "\n" + fileURI(p)
	}
	return []offer{
		{target: "text/uri-list", data: uriList(paths)},
		{target: "x-special/gnome-copied-files", data: []byte(gnome)},
	}
}

// writeTargets lists, for each format, the targets a set offers it as, the
// first being the one stat names. Applications disagree on text: GTK reads
// text/plain;charset=utf-8, X11 clients UTF8_STRING, and others plain
// text/plain, so all three are offered; RTF appears under two MIME types.
var writeTargets = map[weavewire.ClipboardFormat][]string{
	weavewire.ClipboardText:  {"text/plain;charset=utf-8", "UTF8_STRING", "text/plain"},
	weavewire.ClipboardHTML:  {"text/html"},
	weavewire.ClipboardRTF:   {"text/rtf", "application/rtf"},
	weavewire.ClipboardPNG:   {"image/png"},
	weavewire.ClipboardTIFF:  {"image/tiff"},
	weavewire.ClipboardPDF:   {"application/pdf"},
	weavewire.ClipboardFiles: {"text/uri-list", "x-special/gnome-copied-files"},
}

// targetsFor lists, for each format, the targets another application may
// offer it under, preferred first. STRING is Latin-1, read only when nothing
// better is offered.
var targetsFor = map[weavewire.ClipboardFormat][]string{
	weavewire.ClipboardText:  {"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "STRING"},
	weavewire.ClipboardHTML:  {"text/html"},
	weavewire.ClipboardRTF:   {"text/rtf", "application/rtf"},
	weavewire.ClipboardPNG:   {"image/png"},
	weavewire.ClipboardTIFF:  {"image/tiff"},
	weavewire.ClipboardPDF:   {"application/pdf"},
	weavewire.ClipboardFiles: {"text/uri-list", "x-special/gnome-copied-files"},
}

// pickTarget finds the target the clipboard offers format f under. Targets
// are compared case-insensitively and exactly — text/plain;charset=utf-8 is
// not text/plain, which may be in the locale's encoding.
func pickTarget(offered []string, f weavewire.ClipboardFormat) (string, bool) {
	for _, want := range targetsFor[f] {
		for _, o := range offered {
			if strings.EqualFold(normalise(o), normalise(want)) {
				return o, true
			}
		}
	}
	return "", false
}

// normalise drops the whitespace some applications put around a MIME
// parameter ("text/plain; charset=utf-8").
func normalise(t string) string { return strings.ReplaceAll(t, " ", "") }
