//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// dataControlClipboard is the backend in a Wayland session on f.
func dataControlClipboard(t *testing.T, f *fakeCompositor) *clipboard {
	t.Helper()
	c := testClipboard(map[string]string{"WAYLAND_DISPLAY": f.path})
	c.dialWayland = newClipboard().dialWayland
	t.Cleanup(c.closeMechanism)
	m, err := c.mechanism(weavewire.KindClipboardStat)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(*dataControl); !ok {
		t.Fatalf("mechanism %T, want data control", m)
	}
	return c
}

func TestDataControlPrefersTheExtProtocol(t *testing.T) {
	for name, tc := range map[string]struct {
		ext, wlr bool
		want     string
	}{
		"both": {true, true, "ext_data_control_manager_v1"},
		"ext":  {true, false, "ext_data_control_manager_v1"},
		"wlr":  {false, true, "zwlr_data_control_manager_v1"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeCompositor(t, tc.ext, tc.wlr)
			c := dataControlClipboard(t, f)
			if got := c.m.name(); got != tc.want {
				t.Errorf("bound %s, want %s", got, tc.want)
			}
			if s := c.Support(); s.SingleRepresentation {
				t.Error("data control holds one representation")
			}
		})
	}
}

func TestDataControlNeedsTheProtocolAndASeat(t *testing.T) {
	f := newFakeCompositor(t, false, false)
	if _, err := dialDataControl(f.path); !errors.Is(err, errNoDataControl) {
		t.Errorf("no data control: %v", err)
	}
	f = newFakeCompositor(t, true, false)
	f.mu.Lock()
	f.seat = false
	f.mu.Unlock()
	if _, err := dialDataControl(f.path); !errors.Is(err, errNoDataControl) ||
		!strings.Contains(err.Error(), "no seat") {
		t.Errorf("no seat: %v", err)
	}
	if _, err := dialDataControl(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("connected to no socket")
	}
}

// Another application's copy reaches the module with every MIME type it
// offered, and moves the token; the module's set reaches another
// application's paste, every representation of it.
func TestDataControlInterop(t *testing.T) {
	f := newFakeCompositor(t, true, true)
	c := dataControlClipboard(t, f)
	ctx := context.Background()
	before, err := c.Stat(ctx)
	if err != nil || len(before.Formats) != 0 {
		t.Fatalf("empty: %+v, %v", before, err)
	}

	f.setForeign(map[string][]byte{
		"text/html": []byte("<b>x</b>"), "UTF8_STRING": []byte("x"), "image/png": []byte("png"),
	}, "text/html", "UTF8_STRING", "image/png")
	st, err := c.Stat(ctx)
	if err != nil || st.ChangeToken == before.ChangeToken || len(st.Formats) != 3 {
		t.Fatalf("after a foreign copy: %+v, %v", st, err)
	}
	got, err := c.Read(ctx, nil, 1<<20)
	if err != nil || len(got.Items) != 3 || string(got.Items[1].Data) != "<b>x</b>" {
		t.Fatalf("read %+v, %v", got.Items, err)
	}

	set, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("mine")},
		{Format: weavewire.ClipboardRTF, Data: []byte(`{\rtf1}`)},
	})
	if err != nil || len(set.Written) != 2 || set.ChangeToken == st.ChangeToken {
		t.Fatalf("set %+v, %v", set, err)
	}
	for mime, want := range map[string]string{
		"text/plain;charset=utf-8": "mine", "UTF8_STRING": "mine", "text/plain": "mine",
		"text/rtf": `{\rtf1}`, "application/rtf": `{\rtf1}`,
	} {
		if got, err := f.paste(mime); err != nil || string(got) != want {
			t.Errorf("paste %s: %q, %v", mime, got, err)
		}
	}
	if _, err := f.paste("image/png"); !errors.Is(err, errNotOffered) {
		t.Errorf("paste of a type not offered: %v", err)
	}

	// Another copy cancels the module's source.
	f.setForeign(map[string][]byte{"text/plain": []byte("theirs")}, "text/plain")
	d := c.m.(*dataControl)
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		n := len(d.sources)
		d.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancelled source was kept")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, err := d.read(ctx, "image/png"); !errors.Is(err, errNotOffered) {
		t.Errorf("read of a type not offered: %q, %v", got, err)
	}
	f.mu.Lock()
	f.setLocked(nil)
	f.mu.Unlock()
	if st, err := c.Stat(ctx); err != nil || len(st.Formats) != 0 {
		t.Errorf("a cleared selection: %+v, %v", st, err)
	}
	if _, err := d.read(ctx, "text/plain"); !errors.Is(err, errNotOffered) {
		t.Errorf("read with no selection: %v", err)
	}
}

// Object ids the compositor deleted are reused, so the compositor's table of
// this client's objects does not grow with every round trip.
func TestDataControlReusesIDs(t *testing.T) {
	f := newFakeCompositor(t, true, false)
	c := dataControlClipboard(t, f)
	d := c.m.(*dataControl)
	for range 50 {
		if _, _, err := d.offered(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	d.c.idMu.Lock()
	next := d.c.next
	d.c.idMu.Unlock()
	if next > 12 {
		t.Errorf("ids reached %d after 50 round trips", next)
	}
}

// A connection the compositor ends — a protocol error, its seat gone, or the
// compositor itself — breaks the mechanism, and every op on it fails.
func TestDataControlBreaks(t *testing.T) {
	for name, kill := range map[string]func(f *fakeCompositor){
		"protocol error": func(f *fakeCompositor) {
			f.each(func(c *fakeClient) {
				c.send(wlDisplay, displayError, wlArgs{}.uint(3).uint(1).string("bad request"), -1)
			})
		},
		"seat removed": func(f *fakeCompositor) {
			f.each(func(c *fakeClient) {
				for _, d := range c.devices {
					c.send(d, deviceFinished, nil, -1)
				}
			})
		},
		"compositor gone": func(f *fakeCompositor) { f.stop() },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeCompositor(t, true, false)
			c := dataControlClipboard(t, f)
			d := c.m.(*dataControl)
			kill(f)
			select {
			case <-d.done:
			case <-time.After(5 * time.Second):
				t.Fatal("the connection did not end")
			}
			if !d.broken() {
				t.Error("not broken")
			}
			ctx := context.Background()
			if _, _, err := d.offered(ctx); err == nil {
				t.Error("offered succeeded")
			}
			if _, err := d.read(ctx, "text/plain"); err == nil {
				t.Error("read succeeded")
			}
			if _, err := d.own(ctx, []offer{{"text/plain", nil}}); err == nil {
				t.Error("own succeeded")
			}
			if err := d.roundtrip(ctx); err == nil {
				t.Error("roundtrip succeeded")
			}
		})
	}
}

// A compositor that does not answer is a timeout, not a hang.
func TestDataControlRoundtripTimesOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "silent")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c, err := dialWayland(path)
	if err != nil {
		t.Fatal(err)
	}
	d := &dataControl{
		c:       c,
		objects: map[uint32]wlKind{},
		syncs:   map[uint32]chan struct{}{},
		done:    make(chan struct{}),
	}
	t.Cleanup(func() { _ = c.close() })
	if err := d.roundtrip(
		ctx,
	); err == nil ||
		!strings.Contains(err.Error(), "waiting for the compositor") {
		t.Errorf("roundtrip: %v", err)
	}
}

// Malformed messages are protocol errors rather than misreads.
func TestWireDecoding(t *testing.T) {
	m := &wlMessage{args: wlArgs{}.uint(7).string("héllo").uint(9)}
	if v, err := m.uint(); err != nil || v != 7 {
		t.Errorf("uint %d, %v", v, err)
	}
	if s, err := m.string(); err != nil || s != "héllo" {
		t.Errorf("string %q, %v", s, err)
	}
	if v, _ := m.uint(); v != 9 || len(m.args) != 0 {
		t.Errorf("trailing uint %d, %d bytes left", v, len(m.args))
	}
	if _, err := m.uint(); !errors.Is(err, errWlProtocol) {
		t.Errorf("short uint: %v", err)
	}
	for _, args := range []wlArgs{nil, (wlArgs{}).uint(0), (wlArgs{}).uint(40).uint(1)} {
		if _, err := (&wlMessage{args: args}).string(); !errors.Is(err, errWlProtocol) {
			t.Errorf("bad string %x: %v", []byte(args), err)
		}
	}
	if _, err := (&wlMessage{conn: &wlConn{}}).fd(); !errors.Is(err, errWlProtocol) {
		t.Errorf("missing fd: %v", err)
	}
	if got := (wlArgs{}).string("abc"); len(got) != 8 || binary.LittleEndian.Uint32(got) != 4 {
		t.Errorf("encoded %x", []byte(got))
	}
}

// read turns a broken stream into errors: a message smaller than its header,
// and a connection closed.
func TestWireReading(t *testing.T) {
	a, b, err := unixPair()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.close(); _ = b.close() }()
	go func() { _, _ = b.sock.Write([]byte{1, 0, 0, 0, 0, 0, 4, 0}) }() // size 4
	if err := a.read(func(*wlMessage) error { return nil }); !errors.Is(err, errWlProtocol) {
		t.Errorf("tiny message: %v", err)
	}

	// A handler's error ends the read; fds that came with it are queued.
	r, w, _ := os.Pipe()
	defer func() { _ = r.Close() }()
	go func() {
		_ = b.send(5, 0, wlArgs{}.uint(1), int(w.Fd()))
		_ = w.Close()
	}()
	stop := errors.New("stop")
	if err := a.read(func(m *wlMessage) error {
		fd, err := m.fd()
		if err == nil {
			_ = syscall.Close(fd)
		}
		return stop
	}); !errors.Is(err, stop) {
		t.Errorf("handler error: %v", err)
	}
	_ = b.close()
	if err := a.read(func(*wlMessage) error { return nil }); !errors.Is(err, errWlClosed) {
		t.Errorf("closed: %v", err)
	}
	if err := a.send(1, 0, nil, -1); err != nil {
		// a write to a peer that closed may or may not fail at once
		if !errors.Is(err, errWlClosed) {
			t.Errorf("send: %v", err)
		}
	}
	a.queueFDs([]byte("not a control message"))
}

func unixPair() (*wlConn, *wlConn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	conn := func(fd int) (*wlConn, error) {
		f := os.NewFile(uintptr(fd), "wl")
		defer func() { _ = f.Close() }()
		c, err := net.FileConn(f)
		if err != nil {
			return nil, err
		}
		return &wlConn{sock: c.(*net.UnixConn), next: 2}, nil
	}
	a, err := conn(fds[0])
	if err != nil {
		return nil, nil, err
	}
	b, err := conn(fds[1])
	return a, b, err
}

// A large representation goes through the pipe whole, both ways.
func TestDataControlLargeContent(t *testing.T) {
	f := newFakeCompositor(t, true, false)
	c := dataControlClipboard(t, f)
	big := bytes.Repeat([]byte("0123456789"), 300_000)
	if _, err := c.Write(
		context.Background(),
		[]weavewire.ClipboardItem{{Format: weavewire.ClipboardPNG, Data: big}},
	); err != nil {
		t.Fatal(err)
	}
	if got, err := f.paste("image/png"); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("pasted %d bytes, %v", len(got), err)
	}
	got, err := c.Read(
		context.Background(),
		[]weavewire.ClipboardFormat{weavewire.ClipboardPNG},
		1<<30,
	)
	if err != nil || len(got.Items) != 1 || !bytes.Equal(got.Items[0].Data, big) {
		t.Fatalf("read back %d items, %v", len(got.Items), err)
	}
	// Over what one read may hold, it is sized and unread, never cut short.
	defer func(n int64) { maxRead = n }(maxRead)
	maxRead = int64(len(big)) - 1
	over, err := c.Read(
		context.Background(),
		[]weavewire.ClipboardFormat{weavewire.ClipboardPNG},
		1<<30,
	)
	if err != nil || len(over.Items) != 1 || over.Items[0].Data != nil ||
		over.Items[0].Size != int64(len(big)) {
		t.Fatalf("an over-large read: %+v, %v", over.Items, err)
	}
	maxRead = 1 << 30
	if !slices.Contains(c.Support().Formats, weavewire.ClipboardFormatSupport{
		Format: weavewire.ClipboardPNG, Held: true, Native: "image/png",
	}) {
		t.Error("support")
	}
}

// bareDataControl is a data-control client on one end of a socket pair with
// nothing reading its events: handle is called by hand, and peer sees its
// requests.
func bareDataControl(t *testing.T) (*dataControl, *wlConn) {
	t.Helper()
	a, peer, err := unixPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.close(); _ = peer.close() })
	d := &dataControl{
		c:       a,
		objects: map[uint32]wlKind{wlDisplay: 0, 2: kindRegistry, 3: kindDevice, 4: kindSource},
		offers:  map[uint32][]string{},
		sources: map[uint32]map[string][]byte{4: {"text/plain": []byte("x")}},
		syncs:   map[uint32]chan struct{}{},
		done:    make(chan struct{}),
		device:  3,
	}
	return d, peer
}

// Events that do not parse are protocol errors; events for objects already
// gone are ignored.
func TestDataControlEventErrors(t *testing.T) {
	d, _ := bareDataControl(t)
	d.objects[9] = kindOffer
	bad := wlArgs{}.uint(40)
	for name, m := range map[string]*wlMessage{
		"display error":     {object: wlDisplay, opcode: displayError, args: wlArgs{}.uint(1).uint(2).string("no")},
		"delete_id":         {object: wlDisplay, opcode: displayDeleteID},
		"global name":       {object: 2, opcode: registryGlobal},
		"global interface":  {object: 2, opcode: registryGlobal, args: wlArgs{}.uint(1).uint(40)},
		"global version":    {object: 2, opcode: registryGlobal, args: wlArgs{}.uint(1).string("wl_seat")},
		"offer type":        {object: 9, opcode: offerOffer, args: bad},
		"data_offer":        {object: 3, opcode: deviceDataOffer},
		"selection":         {object: 3, opcode: deviceSelection},
		"source send type":  {object: 4, opcode: sourceSend, args: bad},
		"source send no fd": {object: 4, opcode: sourceSend, args: wlArgs{}.string("text/plain")},
	} {
		m.conn = d.c
		if err := d.handle(m); !errors.Is(err, errWlProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := d.handle(&wlMessage{object: 77, conn: d.c}); err != nil {
		t.Errorf("an event for an unknown object: %v", err)
	}
}

// A send for a type the source does not hold gets its pipe closed at once;
// the primary selection is tracked only to release its offers.
func TestDataControlSourceAndPrimaryEvents(t *testing.T) {
	d, _ := bareDataControl(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	rights := syscall.UnixRights(int(w.Fd()))
	d.c.queueFDs(rights)
	_ = w.Close()
	if err := d.handle(&wlMessage{
		object: 4, opcode: sourceSend, args: wlArgs{}.string("image/png"), conn: d.c,
	}); err != nil {
		t.Fatal(err)
	}
	_ = r.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(r); err != nil || len(b) != 0 {
		t.Errorf("a type not held: read %q, %v", b, err)
	}

	for _, id := range []uint32{10, 11} {
		d.objects[id] = kindOffer
	}
	d.sel = 10
	if err := d.handle(
		&wlMessage{object: 3, opcode: devicePrimarySelection, args: wlArgs{}.uint(11), conn: d.c},
	); err != nil {
		t.Fatal(err)
	}
	if err := d.handle(
		&wlMessage{object: 3, opcode: devicePrimarySelection, args: wlArgs{}.uint(10), conn: d.c},
	); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, kept := d.objects[11]
	d.mu.Unlock()
	if d.primary != 10 || kept || d.gen.Load() != 0 {
		t.Errorf("primary %d, offer 11 kept %v, gen %d", d.primary, kept, d.gen.Load())
	}
}

// A source that never writes is a read that times out, not a hang; requests
// on a closed socket fail.
func TestDataControlReadAndOwnFailures(t *testing.T) {
	d, _ := bareDataControl(t)
	d.objects[12] = kindOffer
	d.offers[12] = []string{"text/plain"}
	d.sel = 12
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := d.read(
		ctx,
		"text/plain",
	); err == nil ||
		!strings.Contains(err.Error(), "reading text/plain") {
		t.Errorf("read from a silent source: %v", err)
	}

	_ = d.c.sock.Close()
	if _, err := d.read(context.Background(), "text/plain"); !errors.Is(err, errWlClosed) {
		t.Errorf("read on a closed socket: %v", err)
	}
	if _, err := d.own(
		context.Background(),
		[]offer{{"text/plain", nil}},
	); !errors.Is(
		err,
		errWlClosed,
	) {
		t.Errorf("own on a closed socket: %v", err)
	}
	if err := d.roundtrip(context.Background()); !errors.Is(err, errWlClosed) {
		t.Errorf("roundtrip on a closed socket: %v", err)
	}
}

// A round trip in flight when the connection ends returns why it ended.
func TestDataControlRoundtripWhenTheConnectionEnds(t *testing.T) {
	d, peer := bareDataControl(t)
	go d.run()
	errs := make(chan error, 1)
	go func() { errs <- d.roundtrip(context.Background()) }()
	// The sync reaches the peer, which hangs up instead of answering.
	buf := make([]byte, 64)
	if _, err := peer.sock.Read(buf); err != nil {
		t.Fatal(err)
	}
	_ = peer.close()
	select {
	case err := <-errs:
		if !errors.Is(err, errWlClosed) {
			t.Errorf("roundtrip: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the round trip outlived the connection")
	}
}

// A compositor that hangs up during setup is an error, not a hang.
func TestDataControlSetupHangup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hangup")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 64)
		_, _ = conn.Read(buf)
		_ = conn.Close()
	}()
	if _, err := dialDataControl(path); err == nil {
		t.Error("setup succeeded against a compositor that hung up")
	}
}

// File descriptors that arrived but were never claimed are closed with the
// connection, and a message split across reads is reassembled.
func TestWireFDsAndSplitMessages(t *testing.T) {
	a, b, err := unixPair()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.close() }()
	r, w, _ := os.Pipe()
	defer func() { _ = r.Close() }()
	a.queueFDs(syscall.UnixRights(int(w.Fd())))
	_ = w.Close()
	_ = a.close()
	if len(a.fds) != 0 {
		t.Error("queued fds survived close")
	}

	c, d, err := unixPair()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.close(); _ = d.close() }()
	msg := []byte{5, 0, 0, 0, 0, 0, 12, 0, 7, 0, 0, 0}
	go func() {
		_, _ = d.sock.Write(msg[:6])
		time.Sleep(20 * time.Millisecond)
		_, _ = d.sock.Write(msg[6:])
	}()
	stop := errors.New("stop")
	if err := c.read(func(m *wlMessage) error {
		if v, _ := m.uint(); m.object != 5 || v != 7 {
			t.Errorf("message %+v", m)
		}
		return stop
	}); !errors.Is(err, stop) {
		t.Errorf("read: %v", err)
	}
}

// wl-paste and wl-copy, clients of libwayland, paste what the module holds and
// copy what the module reads, on a real compositor started for the tests.
// Skipped without one or without wl-clipboard.
func TestCompositorWithWlClipboard(t *testing.T) {
	display := os.Getenv("WEAVE_CLIPBOARD_TEST_WAYLAND_DISPLAY")
	if display == "" {
		t.Skip(
			"no compositor for the tests: set WEAVE_CLIPBOARD_TEST_WAYLAND_DISPLAY to one started for them",
		)
	}
	if _, err := exec.LookPath("wl-paste"); err != nil {
		t.Skip("wl-clipboard is not installed")
	}
	c := testClipboard(
		map[string]string{
			"WAYLAND_DISPLAY": display,
			"XDG_RUNTIME_DIR": os.Getenv("XDG_RUNTIME_DIR"),
		},
	)
	c.dialWayland = newClipboard().dialWayland
	t.Cleanup(c.closeMechanism)
	ctx := context.Background()
	big := bytes.Repeat([]byte{0x5a}, 900_000)
	if _, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("from weave")},
		{Format: weavewire.ClipboardHTML, Data: []byte("<b>weave</b>")},
		{Format: weavewire.ClipboardPNG, Data: big},
	}); err != nil {
		t.Fatal(err)
	}
	wl := func(stdin []byte, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), "WAYLAND_DISPLAY="+display)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
			return nil, cmd.Run()
		}
		return cmd.Output()
	}
	types, err := wl(nil, "wl-paste", "--list-types")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"text/plain;charset=utf-8", "UTF8_STRING", "text/html", "image/png"} {
		if !strings.Contains(string(types), want) {
			t.Errorf("wl-paste lists %q, without %s", types, want)
		}
	}
	if out, err := wl(
		nil,
		"wl-paste",
		"--no-newline",
		"--type",
		"text/html",
	); err != nil ||
		string(out) != "<b>weave</b>" {
		t.Errorf("wl-paste html %q, %v", out, err)
	}
	if out, err := wl(
		nil,
		"wl-paste",
		"--type",
		"image/png",
	); err != nil ||
		!bytes.Equal(out, big) {
		t.Errorf("wl-paste read %d bytes of the image, %v", len(out), err)
	}

	before, _ := c.Stat(ctx)
	if _, err := wl([]byte("<i>wl-copy</i>"), "wl-copy", "--type", "text/html"); err != nil {
		t.Fatal(err)
	}
	// wl-copy serves from a process it leaves behind; the module's next set
	// takes the selection back, which ends it.
	t.Cleanup(func() {
		_, _ = c.Write(
			ctx,
			[]weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Data: []byte("done")}},
		)
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := c.Read(ctx, []weavewire.ClipboardFormat{weavewire.ClipboardHTML}, 1<<20)
		if err == nil && len(got.Items) == 1 && string(got.Items[0].Data) == "<i>wl-copy</i>" {
			if got.ChangeToken == before.ChangeToken {
				t.Error("wl-copy's copy left the token unchanged")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never read wl-copy's copy: %+v, %v", got.Items, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
