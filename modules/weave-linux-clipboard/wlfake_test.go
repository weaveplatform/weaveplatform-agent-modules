//go:build linux

package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeCompositor is a Wayland compositor with a seat and data control, as
// little of one as a data-control client meets: the registry, sync, the
// managers of both protocols, devices, sources and offers. It keeps one
// selection, sends every device a new offer when it changes, cancels the
// source it replaces, and passes a receive's file descriptor to the source's
// client as a send, exactly as wlroots does. Tests can also act as another
// application: copy (setForeign) and paste (paste).
type fakeCompositor struct {
	t    *testing.T
	ln   *net.UnixListener
	path string

	// What the registry advertises.
	ext, wlr, seat bool

	mu       sync.Mutex
	clients  []*fakeClient
	sel      *fakeSource
	serverID uint32
	accepted chan *fakeClient
}

type fakeClient struct {
	f       *fakeCompositor
	c       *wlConn
	objects map[uint32]string
	devices []uint32
	sources map[uint32]*fakeSource
	offers  map[uint32]*fakeSource
	done    chan struct{}
}

// fakeSource is a selection: a client's source, or another application's
// data when client is nil.
type fakeSource struct {
	client  *fakeClient
	id      uint32
	mimes   []string
	foreign map[string][]byte
}

const (
	fakeSeatName = 1
	fakeExtName  = 2
	fakeWlrName  = 3
	fakeOther    = 4
)

func newFakeCompositor(t *testing.T, ext, wlr bool) *fakeCompositor {
	t.Helper()
	dir, err := os.MkdirTemp("", "wl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "wayland-test")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCompositor{
		t: t, ln: ln, path: path, ext: ext, wlr: wlr, seat: true,
		serverID: 0xff000000, accepted: make(chan *fakeClient, 8),
	}
	go f.accept()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeCompositor) stop() {
	_ = f.ln.Close()
	f.mu.Lock()
	clients := slices.Clone(f.clients)
	f.mu.Unlock()
	for _, c := range clients {
		_ = c.c.close()
		<-c.done
	}
}

func (f *fakeCompositor) accept() {
	for {
		conn, err := f.ln.AcceptUnix()
		if err != nil {
			return
		}
		c := &fakeClient{
			f: f, c: &wlConn{sock: conn},
			objects: map[uint32]string{1: "wl_display"},
			sources: make(map[uint32]*fakeSource),
			offers:  make(map[uint32]*fakeSource),
			done:    make(chan struct{}),
		}
		f.mu.Lock()
		f.clients = append(f.clients, c)
		f.mu.Unlock()
		f.accepted <- c
		go func() {
			defer close(c.done)
			_ = c.c.read(c.request)
		}()
	}
}

// client waits for the next client to connect.
func (f *fakeCompositor) client() *fakeClient {
	select {
	case c := <-f.accepted:
		return c
	case <-time.After(5 * time.Second):
		f.t.Fatal("no client connected")
		return nil
	}
}

func (c *fakeClient) send(object uint32, opcode uint16, args wlArgs, fd int) {
	_ = c.c.send(object, opcode, args, fd)
}

func (c *fakeClient) request(m *wlMessage) error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	iface := c.objects[m.object]
	switch iface {
	case "wl_display":
		id, _ := m.uint()
		switch m.opcode {
		case displaySync:
			c.send(id, callbackDone, wlArgs{}.uint(0), -1)
			c.send(wlDisplay, displayDeleteID, wlArgs{}.uint(id), -1)
		case displayGetRegistry:
			c.objects[id] = "wl_registry"
			c.send(id, registryGlobal, wlArgs{}.uint(fakeOther).string("wl_compositor").uint(6), -1)
			if c.f.seat {
				c.send(
					id,
					registryGlobal,
					wlArgs{}.uint(fakeSeatName).string("wl_seat").uint(9),
					-1,
				)
			}
			if c.f.ext {
				c.send(
					id,
					registryGlobal,
					wlArgs{}.uint(fakeExtName).string(extDataControl.manager).uint(1),
					-1,
				)
			}
			if c.f.wlr {
				c.send(
					id,
					registryGlobal,
					wlArgs{}.uint(fakeWlrName).string(wlrDataControl.manager).uint(2),
					-1,
				)
			}
		}
	case "wl_registry":
		_, _ = m.uint()
		name, _ := m.string()
		_, _ = m.uint()
		id, _ := m.uint()
		c.objects[id] = name
	case extDataControl.manager, wlrDataControl.manager:
		id, _ := m.uint()
		if m.opcode == managerCreateSource {
			c.objects[id] = "source"
			c.sources[id] = &fakeSource{client: c, id: id}
			return nil
		}
		c.objects[id] = "device"
		c.devices = append(c.devices, id)
		c.offer(id, c.f.sel)
	case "device":
		id, _ := m.uint()
		if m.opcode == deviceSetSelection {
			c.f.setLocked(c.sources[id])
		}
	case "source":
		if m.opcode == sourceOffer {
			mime, _ := m.string()
			s := c.sources[m.object]
			s.mimes = append(s.mimes, mime)
			return nil
		}
		delete(c.sources, m.object)
		delete(c.objects, m.object)
		c.send(wlDisplay, displayDeleteID, wlArgs{}.uint(m.object), -1)
	case "offer":
		if m.opcode == offerDestroy {
			delete(c.offers, m.object)
			delete(c.objects, m.object)
			return nil
		}
		mime, _ := m.string()
		fd, err := m.fd()
		if err != nil {
			return err
		}
		s := c.offers[m.object]
		switch {
		case s == nil || !slices.Contains(s.mimes, mime):
			_ = syscall.Close(fd)
		case s.client == nil:
			w := os.NewFile(uintptr(fd), "receive")
			go func() {
				_, _ = w.Write(s.foreign[mime])
				_ = w.Close()
			}()
		default:
			s.client.send(s.id, sourceSend, wlArgs{}.string(mime), fd)
			_ = syscall.Close(fd)
		}
	}
	return nil
}

// offer tells a device about the selection s, with a new offer.
func (c *fakeClient) offer(device uint32, s *fakeSource) {
	if s == nil {
		c.send(device, deviceSelection, wlArgs{}.uint(0), -1)
		return
	}
	c.f.serverID++
	id := c.f.serverID
	c.objects[id] = "offer"
	c.offers[id] = s
	c.send(device, deviceDataOffer, wlArgs{}.uint(id), -1)
	for _, mime := range s.mimes {
		c.send(id, offerOffer, wlArgs{}.string(mime), -1)
	}
	c.send(device, deviceSelection, wlArgs{}.uint(id), -1)
}

// setLocked makes s the selection, cancelling the source it replaces and
// telling every device. The caller holds mu.
func (f *fakeCompositor) setLocked(s *fakeSource) {
	if old := f.sel; old != nil && old != s && old.client != nil {
		old.client.send(old.id, sourceCancelled, nil, -1)
	}
	f.sel = s
	for _, c := range f.clients {
		for _, d := range c.devices {
			c.offer(d, s)
		}
	}
}

// setForeign copies data as another application would.
func (f *fakeCompositor) setForeign(data map[string][]byte, order ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setLocked(&fakeSource{mimes: order, foreign: data})
}

// paste reads mime from the selection as another application would.
func (f *fakeCompositor) paste(mime string) ([]byte, error) {
	f.mu.Lock()
	s := f.sel
	f.mu.Unlock()
	if s == nil || !slices.Contains(s.mimes, mime) {
		return nil, errNotOffered
	}
	if s.client == nil {
		return s.foreign[mime], nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	s.client.send(s.id, sourceSend, wlArgs{}.string(mime), int(w.Fd()))
	_ = w.Close()
	_ = r.SetReadDeadline(time.Now().Add(5 * time.Second))
	return io.ReadAll(r)
}

// each runs fn on every client.
func (f *fakeCompositor) each(fn func(c *fakeClient)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.clients {
		fn(c)
	}
}
