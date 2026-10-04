//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// testDisplayEnv names an X display started for the tests — Xvfb in CI and in
// `make test-linux-clipboard` — whose clipboard they may own. It is never the
// DISPLAY a developer's desktop session sets: these tests take the clipboard.
const testDisplayEnv = "WEAVE_CLIPBOARD_TEST_DISPLAY"

// testDisplay returns the test display, skipping without one.
func testDisplay(t *testing.T) string {
	t.Helper()
	d := os.Getenv(testDisplayEnv)
	if d == "" {
		t.Skipf("no X display for the tests: set %s to one started for them (Xvfb)", testDisplayEnv)
	}
	return d
}

// dialTestX11 connects the X11 mechanism to the test display.
func dialTestX11(t *testing.T) *x11 {
	t.Helper()
	x, err := dialX11(testDisplay(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(x.close)
	return x
}

// x11Clipboard is the backend in an X session on the test display.
func x11Clipboard(t *testing.T) *clipboard {
	t.Helper()
	c := testClipboard(map[string]string{"DISPLAY": testDisplay(t)})
	c.dialX11 = newClipboard().dialX11
	t.Cleanup(c.closeMechanism)
	m, err := c.mechanism(weavewire.KindClipboardStat)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(*x11); !ok {
		t.Fatalf("mechanism %T, want X11", m)
	}
	return c
}

// rawClient is another X client, for what the module meets from other
// applications: requests it does not expect, and owners that never answer.
type rawClient struct {
	c   *xgb.Conn
	win xproto.Window
}

func newRawClient(t *testing.T) *rawClient {
	t.Helper()
	c, err := xgb.NewConnDisplay(testDisplay(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	win, err := window(c)
	if err != nil {
		t.Fatal(err)
	}
	return &rawClient{c: c, win: win}
}

func (r *rawClient) atom(t *testing.T, name string) xproto.Atom {
	t.Helper()
	reply, err := xproto.InternAtom(r.c, false, uint16(len(name)), name).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return reply.Atom
}

// convert asks the clipboard's owner for target onto prop at time at, and
// returns the property the owner's SelectionNotify names.
func (r *rawClient) convert(
	t *testing.T,
	target, prop xproto.Atom,
	at xproto.Timestamp,
) xproto.Atom {
	t.Helper()
	clip := r.atom(t, "CLIPBOARD")
	xproto.ConvertSelection(r.c, r.win, clip, target, prop, at)
	deadline := time.After(5 * time.Second)
	for {
		evs := make(chan xgb.Event, 1)
		go func() { ev, _ := r.c.WaitForEvent(); evs <- ev }()
		select {
		case ev := <-evs:
			if n, ok := ev.(xproto.SelectionNotifyEvent); ok {
				return n.Property
			}
		case <-deadline:
			t.Fatal("no SelectionNotify")
		}
	}
}

func TestX11Interop(t *testing.T) {
	c := x11Clipboard(t)
	ctx := context.Background()
	res, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("héllo")},
		{Format: weavewire.ClipboardHTML, Data: []byte("<b>héllo</b>")},
	})
	if err != nil || len(res.Written) != 2 {
		t.Fatalf("set %+v, %v", res, err)
	}

	// Another client, reading as any application does.
	other := dialTestX11(t)
	targets, _, err := other.offered(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TARGETS", "TIMESTAMP", "UTF8_STRING", "text/plain;charset=utf-8", "text/html"} {
		if !slices.Contains(targets, want) {
			t.Errorf("targets %q lack %s", targets, want)
		}
	}
	for target, want := range map[string]string{"UTF8_STRING": "héllo", "text/html": "<b>héllo</b>"} {
		if got, err := other.read(ctx, target); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", target, got, err)
		}
	}
	if _, err := other.read(ctx, "image/png"); !errors.Is(err, errNotOffered) {
		t.Errorf("a target not held: %v", err)
	}

	// The other client copies: the module loses the selection, sees the
	// change, and reads the other client's content.
	before, _ := c.Stat(ctx)
	if _, err := other.own(ctx, []offer{{"image/png", []byte("png")}}); err != nil {
		t.Fatal(err)
	}
	mine := c.m.(*x11)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mine.mu.Lock()
		owned := mine.owned
		mine.mu.Unlock()
		if !owned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the module still thinks it owns the clipboard")
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := c.Stat(ctx)
	if err != nil || st.ChangeToken == before.ChangeToken ||
		!slices.Contains(
			st.Formats,
			weavewire.ClipboardFormatInfo{Format: weavewire.ClipboardPNG},
		) {
		t.Fatalf("after another copy: %+v, %v", st, err)
	}
}

// xclip, a client of a different X library, pastes what the module holds and
// copies what the module reads. Skipped where xclip is not installed.
func TestX11WithXclip(t *testing.T) {
	display := testDisplay(t)
	if _, err := exec.LookPath("xclip"); err != nil {
		t.Skip("xclip is not installed")
	}
	c := x11Clipboard(t)
	ctx := context.Background()
	big := bytes.Repeat([]byte{0x5a}, 900_000) // past one chunk: INCR
	if _, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("from weave")},
		{Format: weavewire.ClipboardPNG, Data: big},
	}); err != nil {
		t.Fatal(err)
	}
	xclip := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(
			ctx,
			"xclip",
			append([]string{"-display", display, "-selection", "clipboard"}, args...)...)
		return cmd.Output()
	}
	if out, err := xclip("-o", "-t", "UTF8_STRING"); err != nil || string(out) != "from weave" {
		t.Errorf("xclip read text %q, %v", out, err)
	}
	if out, err := xclip("-o", "-t", "image/png"); err != nil || !bytes.Equal(out, big) {
		t.Errorf("xclip read %d bytes of the image, %v", len(out), err)
	}
	if out, err := xclip(
		"-o",
		"-t",
		"TARGETS",
	); err != nil ||
		!strings.Contains(string(out), "image/png") {
		t.Errorf("xclip targets %q, %v", out, err)
	}

	cmd := exec.CommandContext(
		ctx,
		"xclip",
		"-display",
		display,
		"-selection",
		"clipboard",
		"-t",
		"text/html",
		"-i",
	)
	cmd.Stdin = strings.NewReader("<i>xclip</i>")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(ctx, []weavewire.ClipboardFormat{weavewire.ClipboardHTML}, 1<<20)
	if err != nil || len(got.Items) != 1 || string(got.Items[0].Data) != "<i>xclip</i>" {
		t.Errorf("read xclip's copy: %+v, %v", got.Items, err)
	}
}

// The owner answers what ICCCM asks of it: TIMESTAMP, an obsolete client's
// request with no property, and refusals for a target it does not hold, a
// request from before it owned the selection, and another selection.
func TestX11AnswersWhatICCCMAsks(t *testing.T) {
	x := dialTestX11(t)
	ctx := context.Background()
	if _, err := x.own(ctx, []offer{{"text/plain", []byte("p")}}); err != nil {
		t.Fatal(err)
	}
	r := newRawClient(t)
	prop := r.atom(t, "RAW_PROP")
	if got := r.convert(t, r.atom(t, "TIMESTAMP"), prop, xproto.TimeCurrentTime); got != prop {
		t.Errorf("TIMESTAMP refused")
	}
	reply, err := xproto.GetProperty(r.c, true, r.win, prop, xproto.GetPropertyTypeAny, 0, 1).
		Reply()
	if err != nil || reply.Type != xproto.AtomInteger ||
		xgb.Get32(reply.Value) != uint32(x.ownedAt) {
		t.Errorf("TIMESTAMP %+v, %v", reply, err)
	}
	plain := r.atom(t, "text/plain")
	if got := r.convert(t, plain, xproto.AtomNone, xproto.TimeCurrentTime); got != plain {
		t.Errorf("an obsolete client's request answered on %d, want the target", got)
	}
	if got := r.convert(
		t,
		r.atom(t, "image/gif"),
		prop,
		xproto.TimeCurrentTime,
	); got != xproto.AtomNone {
		t.Error("a target not held was answered")
	}
	if got := r.convert(t, plain, prop, 1); got != xproto.AtomNone {
		t.Error("a request from before the ownership was answered")
	}
}

// An owner that never answers is an empty clipboard to stat, not a hang, and
// a failed read to get.
func TestX11OwnerThatNeverAnswers(t *testing.T) {
	x := dialTestX11(t)
	r := newRawClient(t)
	if err := xproto.SetSelectionOwnerChecked(r.c, r.win, r.atom(t, "CLIPBOARD"),
		xproto.TimeCurrentTime).Check(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	targets, _, err := x.offered(ctx)
	if err != nil || len(targets) != 0 {
		t.Errorf("offered %q, %v", targets, err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := x.read(ctx2, "UTF8_STRING"); !errors.Is(err, errNoOwner) {
		t.Errorf("read: %v", err)
	}
}

// An INCR requestor that stops asking is dropped once idle, and stops being
// listened to.
func TestX11DropsAnAbandonedTransfer(t *testing.T) {
	x := dialTestX11(t)
	ctx := context.Background()
	big := bytes.Repeat([]byte{1}, x.chunk+1)
	if _, err := x.own(ctx, []offer{{"image/png", big}}); err != nil {
		t.Fatal(err)
	}
	r := newRawClient(t)
	prop := r.atom(t, "RAW_PROP")
	if got := r.convert(t, r.atom(t, "image/png"), prop, xproto.TimeCurrentTime); got != prop {
		t.Fatal("refused")
	}
	x.mu.Lock()
	if len(x.transfers) != 1 {
		t.Fatalf("%d transfers", len(x.transfers))
	}
	for _, tr := range x.transfers {
		tr.last = time.Now().Add(-time.Hour)
	}
	x.sweep()
	n := len(x.transfers)
	x.mu.Unlock()
	if n != 0 {
		t.Errorf("%d transfers left after the sweep", n)
	}
	// A property deleted with no transfer for it is ignored.
	x.continueTransfer(transferKey{r.win, prop})
}

// A closed connection breaks the mechanism, and every op on it fails.
func TestX11Closed(t *testing.T) {
	x := dialTestX11(t)
	x.close()
	deadline := time.Now().Add(5 * time.Second)
	for !x.broken() {
		if time.Now().After(deadline) {
			t.Fatal("not broken")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx := context.Background()
	if _, err := x.timestamp(ctx); !errors.Is(err, errX11Closed) {
		t.Errorf("timestamp: %v", err)
	}
	if _, err := x.own(ctx, []offer{{"text/plain", nil}}); err == nil {
		t.Error("own succeeded")
	}
	if _, err := x.read(ctx, "UTF8_STRING"); err == nil {
		t.Error("read succeeded")
	}
	if _, _, err := x.offered(ctx); err == nil {
		t.Error("offered succeeded")
	}
}

func TestX11Dial(t *testing.T) {
	if _, err := dialX11(":4242"); err == nil {
		t.Error("connected to a display that does not exist")
	}
	x := dialTestX11(t)
	if x.single() || x.name() == "" || x.chunk <= 0 || x.chunk > maxTransferChunk {
		t.Errorf("x11 %s single %v chunk %d", x.name(), x.single(), x.chunk)
	}
	if _, err := x.atoms.name(0xffffff); err == nil {
		t.Error("named an atom that does not exist")
	}
}

// A connection that cannot be made, first or second, leaves none open.
func TestX11DialFailures(t *testing.T) {
	display := testDisplay(t)
	orig := xDial
	t.Cleanup(func() { xDial = orig })
	var made []*xgb.Conn
	calls := 0
	xDial = func(d string) (*xgb.Conn, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("refused")
		}
		c, err := orig(d)
		made = append(made, c)
		return c, err
	}
	if _, err := dialX11(display); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
	// The first connection was closed: a request on it gets no reply.
	if _, err := xproto.GetInputFocus(made[0]).Reply(); err == nil {
		t.Error("the first connection was left open")
	}
}

// A requestor window with two transfers is listened to until both are done.
func TestX11ReleasesAWindowOnlyWhenItsTransfersAreDone(t *testing.T) {
	x := dialTestX11(t)
	r := newRawClient(t)
	x.mu.Lock()
	defer x.mu.Unlock()
	a, b := transferKey{r.win, 1}, transferKey{r.win, 2}
	x.transfers[a] = &transfer{last: time.Now().Add(-time.Hour)}
	x.transfers[b] = &transfer{last: time.Now()}
	x.sweep()
	if _, ok := x.transfers[b]; !ok || len(x.transfers) != 1 {
		t.Errorf("transfers %v", x.transfers)
	}
}

// An X error on the reader's connection — a request about a window that is
// gone — is passed over; the next read still works.
func TestX11ReaderPassesOverErrors(t *testing.T) {
	x := dialTestX11(t)
	xproto.DeleteProperty(x.reader, 0x1fffff, xproto.AtomString)
	if _, err := x.own(context.Background(), []offer{{"text/plain", []byte("ok")}}); err != nil {
		t.Fatal(err)
	}
	if got, err := x.read(context.Background(), "text/plain"); err != nil || string(got) != "ok" {
		t.Errorf("read %q, %v", got, err)
	}
}

// A read waiting on an owner that never answers ends when the connection
// closes; an INCR transfer whose owner stops sending ends at the deadline.
func TestX11ReadEnds(t *testing.T) {
	x := dialTestX11(t)
	r := newRawClient(t)
	clip := r.atom(t, "CLIPBOARD")
	if err := xproto.SetSelectionOwnerChecked(r.c, r.win, clip, xproto.TimeCurrentTime).
		Check(); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		_, err := x.read(context.Background(), "UTF8_STRING")
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	x.close()
	if err := <-errs; !errors.Is(err, errX11Closed) && !errors.Is(err, errNoOwner) {
		t.Errorf("read across a close: %v", err)
	}

	// The raw client answers the next request with INCR and nothing more.
	y := dialTestX11(t)
	incr := r.atom(t, "INCR")
	go func() {
		for {
			ev, err := r.c.WaitForEvent()
			if ev == nil && err == nil {
				return
			}
			req, ok := ev.(xproto.SelectionRequestEvent)
			if !ok {
				continue
			}
			size := make([]byte, 4)
			xgb.Put32(size, 1000)
			xproto.ChangeProperty(
				r.c,
				xproto.PropModeReplace,
				req.Requestor,
				req.Property,
				incr,
				32,
				1,
				size,
			)
			n := xproto.SelectionNotifyEvent{
				Time: req.Time, Requestor: req.Requestor, Selection: req.Selection,
				Target: req.Target, Property: req.Property,
			}
			xproto.SendEvent(r.c, false, req.Requestor, 0, string(n.Bytes()))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := y.read(ctx, "image/png"); !errors.Is(err, errNoOwner) {
		t.Errorf("an INCR transfer that stalls: %v", err)
	}
}

// A timestamp asked for with no time left fails rather than waits, and a
// set's wait for its own change counted does too.
func TestX11Deadlines(t *testing.T) {
	x := dialTestX11(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Either the stamp or the deadline may win the race; neither may hang.
	_, _ = x.timestamp(ctx)
	_, _ = x.own(ctx, []offer{{"text/plain", []byte("x")}})
}

func TestNewClipboardDialsReportFailures(t *testing.T) {
	c := newClipboard()
	if _, err := c.dialWayland(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("dialed a Wayland display that does not exist")
	}
	if _, err := c.dialX11(":4243"); err == nil {
		t.Error("dialed an X display that does not exist")
	}
	(&tool{}).close()
}
