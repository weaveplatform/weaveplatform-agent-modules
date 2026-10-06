//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// x11 is the CLIPBOARD selection of an X display, owned and read in pure Go
// over the X protocol (jezek/xgb), with no xclip and no Xlib.
//
// X has no clipboard store: the clipboard is whichever client owns the
// CLIPBOARD selection, and it hands out each representation (target) when a
// paste asks for it. So a set makes this module the owner, holding every
// representation at once and serving each on request — TARGETS, TIMESTAMP and
// the data targets, large ones through the INCR protocol — for as long as it
// owns the selection: until another client copies, or the module stops. That
// is what xclip cannot do: it serves one target per process.
//
// Two connections, because the two halves block differently. The owner
// connection runs an event loop that serves paste requests and counts
// selection changes; a read on the same connection would wait for its own
// SelectionNotify while the loop that must answer it — when the module owns
// the clipboard and is reading itself — is the loop it is blocking. The reader
// connection asks the owner, whoever that is, like any other client.
//
// The change token is a count of selection changes, from XFixes'
// SetSelectionOwner notifications: every copy by any client, the module's own
// sets included, moves it, as the pasteboard and Win32 counters do.
type x11 struct {
	owner, reader       *xgb.Conn
	ownerWin, readerWin xproto.Window
	atoms               *atomCache
	clipboard           xproto.Atom
	// chunk is the most data one property change carries; anything larger
	// goes by INCR. It stays under the server's maximum request size.
	chunk int

	nonce uint64
	gen   atomic.Uint64
	// changed wakes a set waiting to see its own selection change counted.
	changed chan struct{}
	// stamps carries server timestamps from the event loop to timestamp().
	stamps chan xproto.Timestamp

	mu        sync.Mutex
	held      map[xproto.Atom][]byte
	targets   []xproto.Atom // held's targets, in offer order
	owned     bool
	ownedAt   xproto.Timestamp
	transfers map[transferKey]*transfer

	readMu sync.Mutex
	events chan xgb.Event // the reader connection's events

	closeOnce sync.Once
	done      chan struct{}
}

// transferKey names one INCR transfer: the requestor's window and property.
type transferKey struct {
	win  xproto.Window
	prop xproto.Atom
}

// transfer is an INCR transfer in progress: what is left to send.
type transfer struct {
	typ  xproto.Atom
	data []byte
	last time.Time
}

// x11 timeouts: a selection owner that never answers (a hung application)
// would otherwise hang a read, and an INCR requestor that stops asking would
// keep its transfer for the life of the module.
const (
	x11Timeout       = 5 * time.Second
	transferIdle     = 30 * time.Second
	maxTransferChunk = 256 << 10
)

var (
	errNoOwner     = errors.New("the clipboard's owner did not answer")
	errNotOffered  = errors.New("the clipboard does not offer that target")
	errLostOwner   = errors.New("another client took the clipboard as it was set")
	errX11Closed   = errors.New("the X connection closed")
	errNoXFixes    = errors.New("the X server has no XFIXES extension")
	errSelectionOp = errors.New("selection request failed")
)

// xgb logs to stderr — "trying connection without authority info" on every
// connection to a display that needs none — and the module's stderr is its
// log. Every failure that matters reaches the caller as an error instead.
func init() { xgb.Logger = log.New(io.Discard, "", 0) }

// xDial is a seam so tests can fail a connection.
var xDial = xgb.NewConnDisplay

// dialX11 connects to display twice and starts serving. On failure every
// connection it made is closed.
func dialX11(display string) (*x11, error) {
	var conns []*xgb.Conn
	dial := func() (*xgb.Conn, error) {
		c, err := xDial(display)
		if err != nil {
			return nil, fmt.Errorf("connecting to X display %q: %w", display, err)
		}
		conns = append(conns, c)
		return c, nil
	}
	x, err := func() (*x11, error) {
		owner, err := dial()
		if err != nil {
			return nil, err
		}
		// Before the second connection exists: see initXFixes.
		if err := initXFixes(owner); err != nil {
			return nil, err
		}
		reader, err := dial()
		if err != nil {
			return nil, err
		}
		return newX11(owner, reader)
	}()
	if err != nil {
		for _, c := range conns {
			c.Close()
		}
		return nil, err
	}
	return x, nil
}

// xfixesEvents records the event bases whose XFixes events xgb can decode.
var (
	xfixesMu     sync.Mutex
	xfixesEvents = make(map[byte]bool)
)

// initXFixes is xfixes.Init without its data race. Init registers the
// extension's event decoders in xgb's package-wide tables on every call, and
// every open connection's reader reads those tables: a second connection's
// Init writes them under the first's feet. The decoders depend only on the
// server's event base, so they are registered once per base, while this
// connection is the only one it could race with and has asked for no events.
func initXFixes(c *xgb.Conn) error {
	reply, err := xproto.QueryExtension(c, 6, "XFIXES").Reply()
	if err != nil {
		return fmt.Errorf("%w: %w", errNoXFixes, err)
	}
	if !reply.Present {
		return errNoXFixes
	}
	c.ExtLock.Lock()
	c.Extensions["XFIXES"] = reply.MajorOpcode
	c.ExtLock.Unlock()
	xfixesMu.Lock()
	defer xfixesMu.Unlock()
	if !xfixesEvents[reply.FirstEvent] {
		for n, fn := range xgb.NewExtEventFuncs["XFIXES"] {
			xgb.NewEventFuncs[int(reply.FirstEvent)+n] = fn
		}
		for n, fn := range xgb.NewExtErrorFuncs["XFIXES"] {
			xgb.NewErrorFuncs[int(reply.FirstError)+n] = fn
		}
		xfixesEvents[reply.FirstEvent] = true
	}
	return nil
}

func newX11(owner, reader *xgb.Conn) (*x11, error) {
	// XFixes requests are refused until the client says which version it
	// speaks; 2.0 has selection notification.
	if _, err := xfixes.QueryVersion(owner, 5, 0).Reply(); err != nil {
		return nil, fmt.Errorf("%w: %w", errNoXFixes, err)
	}
	x := &x11{
		owner: owner, reader: reader,
		atoms:     newAtomCache(reader),
		changed:   make(chan struct{}, 1),
		stamps:    make(chan xproto.Timestamp, 1),
		held:      make(map[xproto.Atom][]byte),
		transfers: make(map[transferKey]*transfer),
		events:    make(chan xgb.Event, 64),
		done:      make(chan struct{}),
	}
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	x.nonce = binary.LittleEndian.Uint64(nonce[:])

	// A request may carry MaximumRequestLength 4-byte units, its own 24-byte
	// header included.
	maxReq := int(xproto.Setup(owner).MaximumRequestLength)*4 - 1024
	x.chunk = min(maxReq, maxTransferChunk)

	// Interned up front: the event loop names them while it holds mu, and
	// an intern there would be a round trip under the lock.
	for _, name := range []string{
		"CLIPBOARD", "TARGETS", "TIMESTAMP", "INCR", "WEAVE_SELECTION", "WEAVE_TIMESTAMP",
	} {
		if _, err := x.atoms.intern(name); err != nil {
			return nil, err
		}
	}
	x.clipboard, _ = x.atoms.lookup("CLIPBOARD")
	for _, w := range []struct {
		c   *xgb.Conn
		win *xproto.Window
	}{{owner, &x.ownerWin}, {reader, &x.readerWin}} {
		var err error
		if *w.win, err = window(w.c); err != nil {
			return nil, err
		}
	}
	if err := xfixes.SelectSelectionInputChecked(owner, x.ownerWin, x.clipboard,
		xfixes.SelectionEventMaskSetSelectionOwner|
			xfixes.SelectionEventMaskSelectionWindowDestroy|
			xfixes.SelectionEventMaskSelectionClientClose).Check(); err != nil {
		return nil, fmt.Errorf("%w: selecting selection input: %w", errNoXFixes, err)
	}
	go x.serve()
	go x.pump()
	return x, nil
}

// window creates an unmapped input-only window that hears its property
// changes: the requestor of reads, the owner of the selection, and where a
// server timestamp comes from.
func window(c *xgb.Conn) (xproto.Window, error) {
	id, err := xproto.NewWindowId(c)
	if err != nil {
		return 0, fmt.Errorf("allocating a window: %w", err)
	}
	root := xproto.Setup(c).DefaultScreen(c).Root
	if err := xproto.CreateWindowChecked(c, 0, id, root, 0, 0, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, xproto.CwEventMask,
		[]uint32{xproto.EventMaskPropertyChange}).Check(); err != nil {
		return 0, fmt.Errorf("creating a window: %w", err)
	}
	return id, nil
}

func (x *x11) close() {
	x.closeOnce.Do(func() {
		close(x.done)
		x.owner.Close()
		x.reader.Close()
	})
}

func (x *x11) name() string { return "the X11 CLIPBOARD selection" }

func (x *x11) broken() bool {
	select {
	case <-x.done:
		return true
	default:
		return false
	}
}

func (x *x11) single() bool { return false }

func (x *x11) token() uint64 { return x.nonce + x.gen.Load() }

// serve is the owner connection's event loop. It ends when the connection
// closes.
func (x *x11) serve() {
	for {
		ev, xerr := x.owner.WaitForEvent()
		if ev == nil && xerr == nil {
			x.close()
			return
		}
		// An X error here answers a request whose reply nobody waits for —
		// a property change on a requestor window that has since gone — and
		// the request it failed is already abandoned.
		switch e := ev.(type) {
		case xfixes.SelectionNotifyEvent:
			x.gen.Add(1)
			select {
			case x.changed <- struct{}{}:
			default:
			}
		case xproto.SelectionRequestEvent:
			x.answer(e)
		case xproto.SelectionClearEvent:
			x.mu.Lock()
			x.owned = false
			clear(x.held)
			x.targets = nil
			clear(x.transfers)
			x.mu.Unlock()
		case xproto.PropertyNotifyEvent:
			if e.Window == x.ownerWin && e.State == xproto.PropertyNewValue {
				select {
				case x.stamps <- e.Time:
				default:
				}
				continue
			}
			if e.State == xproto.PropertyDelete {
				x.continueTransfer(transferKey{e.Window, e.Atom})
			}
		}
	}
}

// pump hands the reader connection's events to the read waiting for them.
func (x *x11) pump() {
	for {
		ev, xerr := x.reader.WaitForEvent()
		if ev == nil && xerr == nil {
			x.close()
			return
		}
		if ev == nil {
			continue
		}
		select {
		case x.events <- ev:
		case <-x.done:
			return
		}
	}
}

// timestamp is the server's current time, which ICCCM requires a selection
// owner to claim the selection with (CurrentTime would let a stale request
// win a race): the time stamped on a zero-length append to the owner window.
func (x *x11) timestamp(ctx context.Context) (xproto.Timestamp, error) {
	select {
	case <-x.stamps:
	default:
	}
	prop, _ := x.atoms.lookup("WEAVE_TIMESTAMP")
	xproto.ChangeProperty(x.owner, xproto.PropModeAppend, x.ownerWin, prop,
		xproto.AtomString, 8, 0, nil)
	ctx, cancel := context.WithTimeout(ctx, x11Timeout)
	defer cancel()
	select {
	case t := <-x.stamps:
		return t, nil
	case <-x.done:
		return 0, errX11Closed
	case <-ctx.Done():
		return 0, fmt.Errorf("waiting for a server timestamp: %w", ctx.Err())
	}
}

// own makes the module the clipboard's owner, serving offers, and returns the
// token once the change is counted.
func (x *x11) own(ctx context.Context, offers []offer) (uint64, error) {
	held := make(map[xproto.Atom][]byte, len(offers))
	targets := make([]xproto.Atom, 0, len(offers))
	for _, o := range offers {
		a, err := x.atoms.intern(o.target)
		if err != nil {
			return 0, err
		}
		if _, dup := held[a]; !dup {
			targets = append(targets, a)
		}
		held[a] = o.data
	}
	at, err := x.timestamp(ctx)
	if err != nil {
		return 0, err
	}
	select {
	case <-x.changed:
	default:
	}
	before := x.gen.Load()

	x.mu.Lock()
	x.held, x.targets, x.owned, x.ownedAt = held, targets, true, at
	clear(x.transfers)
	x.mu.Unlock()
	if err := xproto.SetSelectionOwnerChecked(x.owner, x.ownerWin, x.clipboard, at).
		Check(); err != nil {
		return 0, fmt.Errorf("%w: SetSelectionOwner: %w", errSelectionOp, err)
	}
	reply, err := xproto.GetSelectionOwner(x.reader, x.clipboard).Reply()
	if err != nil {
		return 0, fmt.Errorf("%w: GetSelectionOwner: %w", errSelectionOp, err)
	}
	if reply.Owner != x.ownerWin {
		return 0, errLostOwner
	}

	// The XFixes notification of this very set is in flight on the owner
	// connection; the token is only right once it is counted.
	ctx, cancel := context.WithTimeout(ctx, x11Timeout)
	defer cancel()
	for x.gen.Load() == before {
		select {
		case <-x.changed:
		case <-x.done:
			return 0, errX11Closed
		case <-ctx.Done():
			return 0, fmt.Errorf("waiting for the selection change: %w", ctx.Err())
		}
	}
	return x.token(), nil
}

// answer serves one paste request, as ICCCM 2.2 describes: the data goes on
// the requestor's property, then a SelectionNotify says where; a refusal is a
// SelectionNotify with no property.
func (x *x11) answer(e xproto.SelectionRequestEvent) {
	prop := e.Property
	if prop == xproto.AtomNone {
		prop = e.Target // an obsolete client: the target names the property
	}
	ok := x.provide(e, prop)
	reply := xproto.SelectionNotifyEvent{
		Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target,
	}
	if ok {
		reply.Property = prop
	}
	xproto.SendEvent(x.owner, false, e.Requestor, 0, string(reply.Bytes()))
}

// provide puts the requested target on the requestor's property, reporting
// whether it could.
func (x *x11) provide(e xproto.SelectionRequestEvent, prop xproto.Atom) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	// A request from before this ownership began is for a selection the
	// module no longer holds in that form.
	if e.Selection != x.clipboard || !x.owned ||
		(e.Time != xproto.TimeCurrentTime && e.Time < x.ownedAt) {
		return false
	}
	targets, _ := x.atoms.lookup("TARGETS")
	timestamp, _ := x.atoms.lookup("TIMESTAMP")
	switch e.Target {
	case targets:
		list := append([]xproto.Atom{targets, timestamp}, x.targets...)
		buf := make([]byte, 4*len(list))
		for i, a := range list {
			xgb.Put32(buf[4*i:], uint32(a))
		}
		xproto.ChangeProperty(x.owner, xproto.PropModeReplace, e.Requestor, prop,
			xproto.AtomAtom, 32, uint32(len(list)), buf) //nolint:gosec // G115: a handful of atoms
		return true
	case timestamp:
		buf := make([]byte, 4)
		xgb.Put32(buf, uint32(x.ownedAt))
		xproto.ChangeProperty(x.owner, xproto.PropModeReplace, e.Requestor, prop,
			xproto.AtomInteger, 32, 1, buf)
		return true
	}
	data, ok := x.held[e.Target]
	if !ok {
		return false
	}
	if len(data) <= x.chunk {
		xproto.ChangeProperty(x.owner, xproto.PropModeReplace, e.Requestor, prop,
			e.Target, 8, uint32(len(data)), data) //nolint:gosec // G115: under x.chunk
		return true
	}

	// INCR: announce the size, then send a chunk each time the requestor
	// deletes the property, and an empty one to end. The requestor's
	// property changes reach this connection only once it asks for them.
	x.sweep()
	incr, _ := x.atoms.lookup("INCR")
	xproto.ChangeWindowAttributes(x.owner, e.Requestor, xproto.CwEventMask,
		[]uint32{xproto.EventMaskPropertyChange})
	x.transfers[transferKey{e.Requestor, prop}] = &transfer{
		typ:  e.Target,
		data: data,
		last: time.Now(),
	}
	size := make([]byte, 4)
	xgb.Put32(size, uint32(len(data))) //nolint:gosec // G115: under maxRead
	xproto.ChangeProperty(x.owner, xproto.PropModeReplace, e.Requestor, prop, incr, 32, 1, size)
	return true
}

// continueTransfer sends the next chunk of the INCR transfer the requestor
// asked for by deleting its property.
func (x *x11) continueTransfer(k transferKey) {
	x.mu.Lock()
	defer x.mu.Unlock()
	t, ok := x.transfers[k]
	if !ok {
		return
	}
	n := min(len(t.data), x.chunk)
	xproto.ChangeProperty(x.owner, xproto.PropModeReplace, k.win, k.prop, t.typ, 8,
		uint32(n), t.data[:n]) //nolint:gosec // G115: under x.chunk
	t.data, t.last = t.data[n:], time.Now()
	if n == 0 {
		// The empty chunk ends the transfer.
		delete(x.transfers, k)
		x.release(k.win)
	}
}

// sweep drops transfers whose requestor stopped asking. The caller holds mu.
func (x *x11) sweep() {
	for k, t := range x.transfers {
		if time.Since(t.last) > transferIdle {
			delete(x.transfers, k)
			x.release(k.win)
		}
	}
}

// release stops listening to a requestor window once no transfer to it is
// left. The caller holds mu.
func (x *x11) release(win xproto.Window) {
	for k := range x.transfers {
		if k.win == win {
			return
		}
	}
	xproto.ChangeWindowAttributes(x.owner, win, xproto.CwEventMask, []uint32{0})
}

// offered lists the targets the clipboard's owner offers, and the token.
func (x *x11) offered(ctx context.Context) ([]string, uint64, error) {
	tok := x.token()
	reply, err := xproto.GetSelectionOwner(x.reader, x.clipboard).Reply()
	if err != nil {
		return nil, 0, fmt.Errorf("%w: GetSelectionOwner: %w", errSelectionOp, err)
	}
	if reply.Owner == xproto.WindowNone {
		return nil, tok, nil
	}
	data, err := x.read(ctx, "TARGETS")
	if errors.Is(err, errNotOffered) || errors.Is(err, errNoOwner) {
		return nil, tok, nil // an owner that cannot list its targets offers nothing usable
	}
	if err != nil {
		return nil, 0, err
	}
	var names []string
	for i := 0; i+4 <= len(data); i += 4 {
		name, err := x.atoms.name(xproto.Atom(xgb.Get32(data[i:])))
		if err == nil && name != "" {
			names = append(names, name)
		}
	}
	return names, tok, nil
}

// read asks the clipboard's owner for target and returns its data, following
// an INCR transfer to the end.
func (x *x11) read(ctx context.Context, target string) ([]byte, error) {
	x.readMu.Lock()
	defer x.readMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, x11Timeout)
	defer cancel()
	t, err := x.atoms.intern(target)
	if err != nil {
		return nil, err
	}
	prop, _ := x.atoms.lookup("WEAVE_SELECTION")
	// Leftovers of a read that timed out would answer this one.
	for drained := false; !drained; {
		select {
		case <-x.events:
		default:
			drained = true
		}
	}
	xproto.ConvertSelection(x.reader, x.readerWin, x.clipboard, t, prop, xproto.TimeCurrentTime)

	notify, err := x.wait(ctx, func(ev xgb.Event) bool {
		n, ok := ev.(xproto.SelectionNotifyEvent)
		return ok && n.Requestor == x.readerWin && n.Target == t
	})
	if err != nil {
		return nil, err
	}
	if notify.(xproto.SelectionNotifyEvent).Property == xproto.AtomNone {
		return nil, errNotOffered
	}
	typ, data, err := x.take(prop)
	if err != nil {
		return nil, err
	}
	if incr, _ := x.atoms.lookup("INCR"); typ != incr {
		return data, nil
	}

	// INCR: deleting the property (take did) asks for each chunk; an empty
	// chunk ends the transfer.
	var out []byte
	for {
		if _, err := x.wait(ctx, func(ev xgb.Event) bool {
			p, ok := ev.(xproto.PropertyNotifyEvent)
			return ok && p.Window == x.readerWin && p.Atom == prop &&
				p.State == xproto.PropertyNewValue
		}); err != nil {
			return nil, err
		}
		_, chunk, err := x.take(prop)
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			return out, nil
		}
		if int64(len(out)+len(chunk)) > maxRead {
			return nil, fmt.Errorf("%s: %w", target, errTooLarge)
		}
		out = append(out, chunk...)
	}
}

// take reads the reader window's property whole and deletes it.
func (x *x11) take(prop xproto.Atom) (xproto.Atom, []byte, error) {
	var (
		typ  xproto.Atom
		data []byte
	)
	for {
		// LongOffset and LongLength count 4-byte units.
		reply, err := xproto.GetProperty(x.reader, false, x.readerWin, prop,
			xproto.GetPropertyTypeAny, uint32(len(data)/4), 1<<24).Reply() //nolint:gosec // G115
		if err != nil {
			return 0, nil, fmt.Errorf("%w: GetProperty: %w", errSelectionOp, err)
		}
		typ = reply.Type
		data = append(data, reply.Value...)
		if reply.BytesAfter == 0 {
			break
		}
	}
	xproto.DeleteProperty(x.reader, x.readerWin, prop)
	return typ, data, nil
}

// wait returns the first reader event match accepts.
func (x *x11) wait(ctx context.Context, match func(xgb.Event) bool) (xgb.Event, error) {
	for {
		select {
		case ev := <-x.events:
			if match(ev) {
				return ev, nil
			}
		case <-x.done:
			return nil, errX11Closed
		case <-ctx.Done():
			return nil, errNoOwner
		}
	}
}

// atomCache interns atoms and names them, once each: both are round trips,
// and a stat names every target the clipboard offers.
type atomCache struct {
	c     *xgb.Conn
	mu    sync.Mutex
	ids   map[string]xproto.Atom
	names map[xproto.Atom]string
}

func newAtomCache(c *xgb.Conn) *atomCache {
	return &atomCache{c: c, ids: make(map[string]xproto.Atom), names: make(map[xproto.Atom]string)}
}

func (a *atomCache) intern(name string) (xproto.Atom, error) {
	if id, ok := a.lookup(name); ok {
		return id, nil
	}
	n := uint16(len(name)) //nolint:gosec // G115: a target name
	reply, err := xproto.InternAtom(a.c, false, n, name).Reply()
	if err != nil {
		return 0, fmt.Errorf("%w: InternAtom %s: %w", errSelectionOp, name, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ids[name], a.names[reply.Atom] = reply.Atom, name
	return reply.Atom, nil
}

// lookup is an atom already interned.
func (a *atomCache) lookup(name string) (xproto.Atom, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.ids[name]
	return id, ok
}

func (a *atomCache) name(id xproto.Atom) (string, error) {
	a.mu.Lock()
	n, ok := a.names[id]
	a.mu.Unlock()
	if ok {
		return n, nil
	}
	reply, err := xproto.GetAtomName(a.c, id).Reply()
	if err != nil {
		return "", fmt.Errorf("%w: GetAtomName %d: %w", errSelectionOp, id, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ids[reply.Name], a.names[id] = id, reply.Name
	return reply.Name, nil
}
