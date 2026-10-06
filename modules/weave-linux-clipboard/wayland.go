//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// dataControl is the Wayland clipboard through a data-control protocol:
// ext-data-control-v1 (wayland-protocols 1.39), or wlr-data-control-unstable-v1
// where the compositor has only that. Both let a client that has no surface
// and no keyboard focus — a background module — read and set the selection,
// which the core wl_data_device protocol never allows. Their requests and
// events are the same, opcode for opcode; only the interface names differ.
//
// As on X11 the clipboard is a source the module owns: a set offers every
// representation's MIME type and the module writes each one to the pipe the
// compositor hands it when something pastes, for as long as its source is the
// selection. The change token counts selection events, which the compositor
// sends to every data-control device on every change, the module's own sets
// included.
//
// Compositors with either protocol: wlroots ones (Sway, Hyprland, river,
// labwc, Wayfire, niri, cage), KDE Plasma (KWin) and COSMIC. GNOME's Mutter
// has neither; there the module uses XWayland's X11 selection, which Mutter
// bridges to Wayland clients, or wl-copy where no X display exists.
type dataControl struct {
	c *wlConn

	// The protocol's interface names, ext or wlr.
	iface protocolNames

	mu      sync.Mutex
	objects map[uint32]wlKind
	offers  map[uint32][]string // an offer's MIME types
	sel     uint32              // the selection's offer, 0 for none
	primary uint32              // the primary selection's offer, unused but released
	sources map[uint32]map[string][]byte
	current uint32 // the module's own source while it is the selection
	globals []wlGlobal
	syncs   map[uint32]chan struct{}

	manager, seat, device uint32

	nonce uint64
	gen   atomic.Uint64
	err   error // why the connection ended
	done  chan struct{}
}

// protocolNames are the interface names of one data-control protocol.
type protocolNames struct{ manager, device, source, offer string }

var (
	extDataControl = protocolNames{
		"ext_data_control_manager_v1", "ext_data_control_device_v1",
		"ext_data_control_source_v1", "ext_data_control_offer_v1",
	}
	wlrDataControl = protocolNames{
		"zwlr_data_control_manager_v1", "zwlr_data_control_device_v1",
		"zwlr_data_control_source_v1", "zwlr_data_control_offer_v1",
	}
)

// wlKind is what an object id names, so its events can be read.
type wlKind int

const (
	kindRegistry wlKind = iota + 1
	kindCallback
	kindSeat
	kindManager
	kindDevice
	kindSource
	kindOffer
)

type wlGlobal struct {
	name    uint32
	iface   string
	version uint32
}

// Requests and events, by opcode.
const (
	displaySync, displayGetRegistry = 0, 1
	displayError, displayDeleteID   = 0, 1
	registryBind                    = 0
	registryGlobal                  = 0
	callbackDone                    = 0

	managerCreateSource, managerGetDevice         = 0, 1
	deviceSetSelection                            = 0
	deviceDataOffer, deviceSelection              = 0, 1
	deviceFinished, devicePrimarySelection        = 2, 3
	sourceOffer, sourceDestroy                    = 0, 1
	sourceSend, sourceCancelled                   = 0, 1
	offerReceive, offerDestroy                    = 0, 1
	offerOffer                                    = 0
	wlDisplay                              uint32 = 1
)

var errNoDataControl = errors.New("the compositor offers no data-control protocol")

// wlTimeout bounds a round trip and a read: a compositor or source client
// that never answers would otherwise hang the op.
const wlTimeout = 5 * time.Second

// dialDataControl connects to the compositor at path and binds a data-control
// device on its first seat. A compositor with no data-control protocol is
// errNoDataControl.
func dialDataControl(path string) (*dataControl, error) {
	c, err := dialWayland(path)
	if err != nil {
		return nil, err
	}
	d := &dataControl{
		c:       c,
		objects: map[uint32]wlKind{wlDisplay: 0},
		offers:  make(map[uint32][]string),
		sources: make(map[uint32]map[string][]byte),
		syncs:   make(map[uint32]chan struct{}),
		done:    make(chan struct{}),
	}
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	d.nonce = binary.LittleEndian.Uint64(nonce[:])
	go d.run()
	if err := d.setup(); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

func (d *dataControl) setup() error {
	registry := d.object(kindRegistry)
	if err := d.c.send(wlDisplay, displayGetRegistry, wlArgs{}.uint(registry), -1); err != nil {
		return err
	}
	if err := d.roundtrip(context.Background()); err != nil {
		return err
	}
	d.mu.Lock()
	globals := slices.Clone(d.globals)
	d.mu.Unlock()
	find := func(iface string) (wlGlobal, bool) {
		i := slices.IndexFunc(globals, func(g wlGlobal) bool { return g.iface == iface })
		if i < 0 {
			return wlGlobal{}, false
		}
		return globals[i], true
	}
	manager, ok := find(extDataControl.manager)
	d.iface = extDataControl
	if !ok {
		manager, ok = find(wlrDataControl.manager)
		d.iface = wlrDataControl
	}
	if !ok {
		return errNoDataControl
	}
	seat, ok := find("wl_seat")
	if !ok {
		return fmt.Errorf("%w: the compositor has no seat", errNoDataControl)
	}
	// Version 1 of each: the primary selection (wlr v2) is not the clipboard.
	d.manager, d.seat, d.device = d.object(kindManager), d.object(kindSeat), d.object(kindDevice)
	reqs := []wlRequest{
		{
			registry,
			registryBind,
			wlArgs{}.uint(manager.name).string(manager.iface).uint(1).uint(d.manager),
		},
		{registry, registryBind, wlArgs{}.uint(seat.name).string(seat.iface).uint(1).uint(d.seat)},
		{d.manager, managerGetDevice, wlArgs{}.uint(d.device).uint(d.seat)},
	}
	if err := d.sendAll(reqs); err != nil {
		return err
	}
	// The device's first selection event, the clipboard as it is now,
	// arrives before this round trip ends.
	return d.roundtrip(context.Background())
}

// wlRequest is one request without a file descriptor.
type wlRequest struct {
	object uint32
	opcode uint16
	args   wlArgs
}

// sendAll sends reqs in order, stopping at the first that fails.
func (d *dataControl) sendAll(reqs []wlRequest) error {
	for _, r := range reqs {
		if err := d.c.send(r.object, r.opcode, r.args, -1); err != nil {
			return err
		}
	}
	return nil
}

// object allocates an id for an object of kind.
func (d *dataControl) object(kind wlKind) uint32 {
	id := d.c.newID()
	d.mu.Lock()
	d.objects[id] = kind
	d.mu.Unlock()
	return id
}

func (d *dataControl) close() {
	_ = d.c.close()
	<-d.done
}

func (d *dataControl) name() string { return d.iface.manager }

func (d *dataControl) single() bool { return false }

func (d *dataControl) broken() bool { return d.alive() != nil }

func (d *dataControl) token() uint64 { return d.nonce + d.gen.Load() }

// alive reports why the connection ended, if it has.
func (d *dataControl) alive() error {
	select {
	case <-d.done:
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.err // read ends only with an error
	default:
		return nil
	}
}

// run reads events until the connection ends.
func (d *dataControl) run() {
	err := d.c.read(d.handle)
	d.mu.Lock()
	d.err = err
	// done closes before the round trips are woken: one woken first would
	// find the connection still alive and report its sync as answered.
	close(d.done)
	for _, ch := range d.syncs {
		close(ch)
	}
	clear(d.syncs)
	d.mu.Unlock()
}

// roundtrip waits until the compositor has handled every request sent so
// far, and the client every event it sent before answering.
func (d *dataControl) roundtrip(ctx context.Context) error {
	id := d.object(kindCallback)
	ch := make(chan struct{})
	d.mu.Lock()
	d.syncs[id] = ch
	d.mu.Unlock()
	if err := d.c.send(wlDisplay, displaySync, wlArgs{}.uint(id), -1); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, wlTimeout)
	defer cancel()
	select {
	case <-ch:
		if err := d.alive(); err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for the compositor: %w", ctx.Err())
	}
}

// handle reads one event.
func (d *dataControl) handle(m *wlMessage) error {
	d.mu.Lock()
	kind, ok := d.objects[m.object]
	d.mu.Unlock()
	if !ok {
		return nil // an object already destroyed; its events are moot
	}
	switch {
	case m.object == wlDisplay && m.opcode == displayError:
		obj, _ := m.uint()
		code, _ := m.uint()
		msg, _ := m.string()
		return fmt.Errorf("%w: object %d, code %d: %s", errWlProtocol, obj, code, msg)
	case m.object == wlDisplay && m.opcode == displayDeleteID:
		id, err := m.uint()
		if err != nil {
			return err
		}
		d.mu.Lock()
		delete(d.objects, id)
		d.mu.Unlock()
		d.c.release(id)
	case kind == kindRegistry && m.opcode == registryGlobal:
		var g wlGlobal
		var err error
		if g.name, err = m.uint(); err != nil {
			return err
		}
		if g.iface, err = m.string(); err != nil {
			return err
		}
		if g.version, err = m.uint(); err != nil {
			return err
		}
		d.mu.Lock()
		d.globals = append(d.globals, g)
		d.mu.Unlock()
	case kind == kindCallback && m.opcode == callbackDone:
		d.mu.Lock()
		if ch, ok := d.syncs[m.object]; ok {
			close(ch)
			delete(d.syncs, m.object)
		}
		d.mu.Unlock()
	case kind == kindDevice:
		return d.deviceEvent(m)
	case kind == kindOffer && m.opcode == offerOffer:
		mime, err := m.string()
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.offers[m.object] = append(d.offers[m.object], mime)
		d.mu.Unlock()
	case kind == kindSource:
		return d.sourceEvent(m)
	}
	return nil
}

func (d *dataControl) deviceEvent(m *wlMessage) error {
	switch m.opcode {
	case deviceDataOffer:
		// A new offer, its id chosen by the compositor; its MIME types follow
		// as offer events, then the selection event that uses it.
		id, err := m.uint()
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.objects[id] = kindOffer
		d.offers[id] = nil
		d.mu.Unlock()
	case deviceSelection, devicePrimarySelection:
		id, err := m.uint()
		if err != nil {
			return err
		}
		d.mu.Lock()
		old := d.sel
		if m.opcode == devicePrimarySelection {
			old, d.primary = d.primary, id
		} else {
			d.sel = id
		}
		stale := old != 0 && old != d.sel && old != d.primary
		d.mu.Unlock()
		if m.opcode == deviceSelection {
			d.gen.Add(1)
		}
		if stale {
			d.destroyOffer(old)
		}
	case deviceFinished:
		return fmt.Errorf(
			"%w: the data-control device was destroyed (its seat went away)",
			errWlClosed,
		)
	}
	return nil
}

// destroyOffer releases an offer that is no longer any selection.
func (d *dataControl) destroyOffer(id uint32) {
	d.mu.Lock()
	delete(d.objects, id)
	delete(d.offers, id)
	d.mu.Unlock()
	_ = d.c.send(id, offerDestroy, nil, -1)
}

func (d *dataControl) sourceEvent(m *wlMessage) error {
	switch m.opcode {
	case sourceSend:
		mime, err := m.string()
		if err != nil {
			return err
		}
		fd, err := m.fd()
		if err != nil {
			return err
		}
		d.mu.Lock()
		data, ok := d.sources[m.object][mime]
		d.mu.Unlock()
		f := os.NewFile(uintptr(fd), "wayland-send")
		if !ok {
			_ = f.Close()
			return nil
		}
		// Written aside: a paste of a large image must not hold up the
		// events behind it, and the reader may be this module.
		go func() {
			_ = f.SetWriteDeadline(time.Now().Add(wlTimeout))
			_, _ = f.Write(data)
			_ = f.Close()
		}()
	case sourceCancelled:
		// Another client set the selection, or this source was replaced.
		d.mu.Lock()
		delete(d.sources, m.object)
		if d.current == m.object {
			d.current = 0
		}
		d.mu.Unlock()
		return d.c.send(m.object, sourceDestroy, nil, -1)
	}
	return nil
}

// offered lists the selection's MIME types, and the token.
func (d *dataControl) offered(ctx context.Context) ([]string, uint64, error) {
	if err := d.alive(); err != nil {
		return nil, 0, err
	}
	// Events already sent — a copy a moment ago — are handled first.
	if err := d.roundtrip(ctx); err != nil {
		return nil, 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.offers[d.sel]), d.token(), nil
}

// read asks the selection's owner for mime through a pipe.
func (d *dataControl) read(ctx context.Context, mime string) ([]byte, error) {
	if err := d.alive(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	sel := d.sel
	offered := slices.Contains(d.offers[sel], mime)
	d.mu.Unlock()
	if sel == 0 || !offered {
		return nil, errNotOffered
	}
	r, w, err := pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	err = d.c.send(sel, offerReceive, wlArgs{}.string(mime), int(w.Fd()))
	// The compositor has its own copy of the write end now; the source's
	// close of it is the end of the data only once this one is closed too.
	_ = w.Close()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wlTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = r.SetReadDeadline(deadline)
	data, err := io.ReadAll(io.LimitReader(r, maxRead+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s from the clipboard: %w", mime, err)
	}
	if int64(len(data)) > maxRead {
		return nil, fmt.Errorf("%s: %w", mime, errTooLarge)
	}
	return data, nil
}

// own offers offers as the selection, and returns the token once the
// compositor has made it the selection.
func (d *dataControl) own(ctx context.Context, offers []offer) (uint64, error) {
	if err := d.alive(); err != nil {
		return 0, err
	}
	src := d.object(kindSource)
	held := make(map[string][]byte, len(offers))
	var mimes []string
	for _, o := range offers {
		if _, dup := held[o.target]; !dup {
			mimes = append(mimes, o.target)
		}
		held[o.target] = o.data
	}
	d.mu.Lock()
	d.sources[src] = held
	d.mu.Unlock()
	reqs := []wlRequest{{d.manager, managerCreateSource, wlArgs{}.uint(src)}}
	for _, mime := range mimes {
		reqs = append(reqs, wlRequest{src, sourceOffer, wlArgs{}.string(mime)})
	}
	reqs = append(reqs, wlRequest{d.device, deviceSetSelection, wlArgs{}.uint(src)})
	if err := d.sendAll(reqs); err != nil {
		return 0, err
	}
	d.mu.Lock()
	d.current = src
	d.mu.Unlock()
	// The selection event for this source arrives before the round trip
	// ends, so the token counts it.
	if err := d.roundtrip(ctx); err != nil {
		return 0, err
	}
	return d.token(), nil
}
