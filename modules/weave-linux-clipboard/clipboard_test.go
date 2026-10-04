//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// memMech is a clipboard mechanism in memory, for what clipboard.go does
// around any mechanism: it holds the offers of the last own, counts sets in
// its token and can be made to fail or break.
type memMech struct {
	mu       sync.Mutex
	label    string
	one      bool
	offers   []offer
	tok      uint64
	fail     error
	dead     bool
	closed   bool
	unreadOf string // a target listed but unreadable
}

func (m *memMech) name() string { return m.label }
func (m *memMech) single() bool { return m.one }

func (m *memMech) broken() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dead
}

func (m *memMech) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
}

func (m *memMech) offered(context.Context) ([]string, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, 0, m.fail
	}
	targets := make([]string, 0, len(m.offers))
	for _, o := range m.offers {
		targets = append(targets, o.target)
	}
	return targets, m.tok, nil
}

func (m *memMech) read(_ context.Context, target string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if target == m.unreadOf {
		return nil, errNotOffered
	}
	for _, o := range m.offers {
		if o.target == target {
			return o.data, nil
		}
	}
	return nil, errNotOffered
}

func (m *memMech) own(_ context.Context, offers []offer) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, m.fail
	}
	m.offers = offers
	m.tok++
	return m.tok, nil
}

// withMech is the backend over m, in an X session.
func withMech(m mechanism) *clipboard {
	c := testClipboard(map[string]string{"DISPLAY": ":0"})
	c.dialX11 = func(string) (mechanism, error) { return m, nil }
	return c
}

func TestDetectPrefersDataControlThenX11ThenTheTools(t *testing.T) {
	installFake(t)
	runtime := map[string]string{"WAYLAND_DISPLAY": "wayland-0", "XDG_RUNTIME_DIR": "/run/user/1"}
	both := map[string]string{
		"WAYLAND_DISPLAY": "wayland-0",
		"DISPLAY":         ":0",
		"XDG_RUNTIME_DIR": "/run/user/1",
	}
	for name, tc := range map[string]struct {
		env               map[string]string
		wayland, x11      bool
		tools             bool
		want, unsupported string
	}{
		"data control": {env: both, wayland: true, x11: true, tools: true, want: "data control"},
		"xwayland under a compositor without data control": {
			env: both, x11: true, tools: true, want: "x11",
		},
		"x11":       {env: map[string]string{"DISPLAY": ":0"}, x11: true, want: "x11"},
		"the tools": {env: runtime, tools: true, want: "wl-copy"},
		"wayland without anything": {
			env:         runtime,
			unsupported: "no data-control protocol; wl-clipboard (wl-paste and wl-copy) is not installed",
		},
		"wayland without a runtime directory": {
			env:         map[string]string{"WAYLAND_DISPLAY": "wayland-0"},
			unsupported: "XDG_RUNTIME_DIR is unset",
		},
		"x11 without xfixes": {env: map[string]string{"DISPLAY": ":0"}, unsupported: "XFIXES"},
		"no display": {
			env: map[string]string{}, unsupported: "WAYLAND_DISPLAY and DISPLAY are both unset",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := testClipboard(tc.env)
			var dialed string
			if tc.wayland {
				c.dialWayland = func(path string) (mechanism, error) {
					dialed = path
					return &memMech{label: "data control"}, nil
				}
			}
			if tc.x11 {
				c.dialX11 = func(string) (mechanism, error) { return &memMech{label: "x11"}, nil }
			}
			c.look = func(string) bool { return tc.tools }
			m, err := c.mechanism(weavewire.KindClipboardStat)
			if tc.unsupported != "" {
				u, ok := errors.AsType[*weavewire.UnsupportedError](err)
				if !ok || !strings.Contains(u.Reason, tc.unsupported) {
					t.Fatalf("err = %v, want unsupported naming %q", err, tc.unsupported)
				}
				return
			}
			if err != nil || m.name() != tc.want {
				t.Fatalf("mechanism %v, %v; want %s", m, err, tc.want)
			}
			if tc.wayland && dialed != "/run/user/1/wayland-0" {
				t.Errorf("dialed %q", dialed)
			}
			if again, _ := c.mechanism(weavewire.KindClipboardGet); again != m {
				t.Error("a second op connected again")
			}
		})
	}
}

// A WAYLAND_DISPLAY that is a path is the socket itself.
func TestWaylandDisplayMayBeAPath(t *testing.T) {
	if p, err := wlSocket("/tmp/x/wayland-1", ""); err != nil || p != "/tmp/x/wayland-1" {
		t.Errorf("socket %q, %v", p, err)
	}
}

// With no display every op is an unsupported answer naming what is missing.
func TestWithoutADisplayEveryOpIsUnsupported(t *testing.T) {
	c := testClipboard(map[string]string{})
	ctx := context.Background()
	_, statErr := c.Stat(ctx)
	_, readErr := c.Read(ctx, nil, 1<<20)
	_, writeErr := c.Write(ctx, []weavewire.ClipboardItem{{Format: weavewire.ClipboardText}})
	for kind, err := range map[string]error{
		weavewire.KindClipboardStat: statErr,
		weavewire.KindClipboardGet:  readErr,
		weavewire.KindClipboardSet:  writeErr,
	} {
		u, ok := errors.AsType[*weavewire.UnsupportedError](err)
		if !ok || u.Kind != kind || !strings.Contains(u.Reason, "DISPLAY") {
			t.Errorf("%s: err = %v, want unsupported naming the missing display", kind, err)
		}
	}
}

// A set offers every representation under every target applications read it
// from, the files as both URI lists, and reports each format written.
func TestWriteOffersEveryRepresentation(t *testing.T) {
	m := &memMech{label: "x11"}
	c := withMech(m)
	res, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("hi")},
		{Format: weavewire.ClipboardRTF, Data: []byte(`{\rtf1}`)},
		{Format: weavewire.ClipboardFiles, Name: "a.txt", Data: []byte("a")},
		{Format: weavewire.ClipboardPNG, Data: []byte("png")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []weavewire.ClipboardFormat{
		weavewire.ClipboardFiles,
		weavewire.ClipboardPNG,
		weavewire.ClipboardRTF,
		weavewire.ClipboardText,
	}; !slices.Equal(res.Written, want) || res.ChangeToken != 1 {
		t.Fatalf("set %+v, want %v at 1", res, want)
	}
	targets := make([]string, 0, len(m.offers))
	for _, o := range m.offers {
		targets = append(targets, o.target)
	}
	if want := []string{
		"text/uri-list", "x-special/gnome-copied-files", "image/png", "text/rtf", "application/rtf",
		"text/plain;charset=utf-8", "UTF8_STRING", "text/plain",
	}; !slices.Equal(targets, want) {
		t.Errorf("offered %q, want %q", targets, want)
	}
	uris, gnome := string(m.offers[0].data), string(m.offers[1].data)
	if !strings.HasSuffix(uris, "/a.txt\r\n") ||
		gnome != "copy\n"+strings.TrimSuffix(uris, "\r\n") {
		t.Errorf("uri-list %q, gnome %q", uris, gnome)
	}
}

// Stat lists the formats offered without reading them, files sized from the
// filesystem; a get reads what was asked for, a file over the cap sized and
// empty, and leaves out what cannot be read.
func TestStatAndRead(t *testing.T) {
	dir := t.TempDir()
	small, big := filepath.Join(dir, "small.txt"), filepath.Join(dir, "big.bin")
	if err := os.WriteFile(small, []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &memMech{label: "x11", tok: 9, unreadOf: "image/png", offers: []offer{
		{"TARGETS", nil},
		{"SAVE_TARGETS", nil},
		{"UTF8_STRING", []byte("small.txt big.bin")},
		{"application/rtf", []byte(`{\rtf1}`)},
		{"image/png", []byte("\x89PNG")},
		{
			"text/uri-list",
			[]byte(
				"# copied\r\nfile://" + small + "\r\nfile://" + big + "\r\nfile://" + dir + "\r\n",
			),
		},
	}}
	c := withMech(m)
	ctx := context.Background()
	st, err := c.Stat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []weavewire.ClipboardFormatInfo{
		{Format: weavewire.ClipboardFiles, Size: 102, Count: 2},
		{Format: weavewire.ClipboardPNG},
		{Format: weavewire.ClipboardRTF},
		{Format: weavewire.ClipboardText},
	}
	if !slices.Equal(st.Formats, want) || st.ChangeToken != 9 {
		t.Errorf("stat %+v, want %+v at 9", st, want)
	}

	all, err := c.Read(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(all.Items))
	for _, it := range all.Items {
		got = append(got, string(it.Format)+":"+it.Name+":"+string(it.Data))
	}
	if want := []string{
		"files:small.txt:ab",
		"files:big.bin:",
		`text/rtf::{\rtf1}`,
		"text/plain::small.txt big.bin",
	}; !slices.Equal(got, want) || all.ChangeToken != 9 {
		t.Errorf("items = %q at %d, want %q", got, all.ChangeToken, want)
	}
	if all.Items[1].Size != 100 {
		t.Errorf("omitted file size = %d, want 100", all.Items[1].Size)
	}
	text, err := c.Read(ctx, []weavewire.ClipboardFormat{weavewire.ClipboardText}, 1<<20)
	if err != nil || len(text.Items) != 1 {
		t.Errorf("text only: %+v, %v", text.Items, err)
	}

	// A file list that names nothing copyable, or cannot be read, is no files.
	m.offers = []offer{{"text/uri-list", []byte("file://" + dir + "\r\n")}}
	if st, err := c.Stat(ctx); err != nil || len(st.Formats) != 0 {
		t.Errorf("stat of a directory: %+v, %v", st, err)
	}
	m.unreadOf = "text/uri-list"
	if st, err := c.Stat(ctx); err != nil || len(st.Formats) != 0 {
		t.Errorf("stat of an unreadable list: %+v, %v", st, err)
	}
}

// A mechanism whose op failed is kept; one whose connection ended is dropped,
// closed, and the next op connects again.
func TestABrokenConnectionIsMadeAgain(t *testing.T) {
	first := &memMech{label: "first", fail: errX11Closed}
	second := &memMech{label: "second"}
	dials := []mechanism{first, second}
	c := testClipboard(map[string]string{"DISPLAY": ":0"})
	c.dialX11 = func(string) (mechanism, error) {
		m := dials[0]
		dials = dials[1:]
		return m, nil
	}
	ctx := context.Background()
	if _, err := c.Stat(ctx); !errors.Is(err, errX11Closed) {
		t.Fatalf("stat: %v", err)
	}
	if c.m != first {
		t.Fatal("a mechanism that failed but is not broken was dropped")
	}
	first.mu.Lock()
	first.dead = true
	first.mu.Unlock()
	if _, err := c.Stat(ctx); err != nil {
		t.Fatalf("stat after reconnecting: %v", err)
	}
	if c.m != second || !first.closed {
		t.Fatalf("mechanism %v, first closed %v", c.m, first.closed)
	}

	for name, op := range map[string]func(*clipboard) error{
		"stat": func(c *clipboard) error { _, err := c.Stat(ctx); return err },
		"read": func(c *clipboard) error { _, err := c.Read(ctx, nil, 1); return err },
		"write": func(c *clipboard) error {
			_, err := c.Write(ctx, []weavewire.ClipboardItem{{Format: weavewire.ClipboardText}})
			return err
		},
	} {
		m := &memMech{label: "dying", fail: errWlClosed}
		c := withMech(m)
		if _, err := c.mechanism(weavewire.KindClipboardStat); err != nil {
			t.Fatal(err)
		}
		m.dead = true
		c.m = m // found broken only by the op that fails on it
		if err := op(c); err == nil {
			t.Fatalf("%s on a dead connection succeeded", name)
		}
		if c.m != nil || !m.closed {
			t.Errorf("%s: the dead mechanism was kept", name)
		}
	}
}

func TestSupport(t *testing.T) {
	c := withMech(&memMech{label: "x11"})
	if s := c.Support(); s.SingleRepresentation || len(s.Formats) != 7 {
		t.Fatalf("before connecting: %+v", s)
	}
	if _, err := c.mechanism(weavewire.KindClipboardStat); err != nil {
		t.Fatal(err)
	}
	for _, f := range c.Support().Formats {
		if !f.Held || f.Native != writeTargets[f.Format][0] {
			t.Errorf("%+v", f)
		}
	}
	single := withMech(&memMech{label: "wl-copy", one: true})
	if _, err := single.mechanism(weavewire.KindClipboardStat); err != nil {
		t.Fatal(err)
	}
	if s := single.Support(); !s.SingleRepresentation ||
		!strings.Contains(s.Limitation, "wl-copy") {
		t.Errorf("support %+v", s)
	}
}

// Through the tools, a set keeps the richest representation, under the one
// target wl-copy offers, and every set moves the token.
func TestToolsHoldTheRichestRepresentation(t *testing.T) {
	f := installFake(t)
	c := toolClipboard(t)
	ctx := context.Background()
	res, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("hi")},
		{Format: weavewire.ClipboardHTML, Data: []byte("<i>hi</i>")},
	})
	if err != nil ||
		!slices.Equal(res.Written, []weavewire.ClipboardFormat{weavewire.ClipboardHTML}) {
		t.Fatalf("set %+v, %v", res, err)
	}
	if got, ok := f.held(t, "text/html"); !ok || got != "<i>hi</i>" {
		t.Errorf("clipboard holds %q", got)
	}
	st, _ := c.Stat(ctx)
	if st.ChangeToken != res.ChangeToken {
		t.Errorf("set token %d, stat afterwards %d", res.ChangeToken, st.ChangeToken)
	}
	again, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardHTML, Data: []byte("<i>hi</i>")},
	})
	if err != nil || again.ChangeToken == res.ChangeToken {
		t.Errorf(
			"the same set twice: tokens %d and %d, %v",
			res.ChangeToken,
			again.ChangeToken,
			err,
		)
	}
	text, err := c.Write(
		ctx,
		[]weavewire.ClipboardItem{{Format: weavewire.ClipboardText, Data: []byte("t")}},
	)
	if err != nil ||
		!slices.Equal(text.Written, []weavewire.ClipboardFormat{weavewire.ClipboardText}) {
		t.Fatalf("text: %+v, %v", text, err)
	}
	if got, _ := f.held(t, "text/plain;charset=utf-8"); got != "t" {
		t.Errorf("text held as %q", got)
	}
	if s := c.Support(); !s.SingleRepresentation {
		t.Error("the tools hold more than one representation")
	}
}

// Through the tools, the token is a digest of the content: an image copied
// over an image, with the same targets, still moves it.
func TestToolTokenSeesNewContent(t *testing.T) {
	f := installFake(t)
	c := toolClipboard(t)
	ctx := context.Background()
	empty, err := c.Stat(ctx)
	if err != nil || len(empty.Formats) != 0 {
		t.Fatalf("empty clipboard: %+v, %v", empty, err)
	}
	f.offer(
		t,
		[]string{"SAVE_TARGETS"},
		map[string]string{"image/png": "\x89PNG", "UTF8_STRING": "x"},
	)
	first, _ := c.Stat(ctx)
	again, _ := c.Stat(ctx)
	f.offer(
		t,
		[]string{"SAVE_TARGETS"},
		map[string]string{"image/png": "\x89PNG2", "UTF8_STRING": "x"},
	)
	changed, _ := c.Stat(ctx)
	if first.ChangeToken == empty.ChangeToken || again.ChangeToken != first.ChangeToken ||
		changed.ChangeToken == first.ChangeToken {
		t.Errorf("tokens: empty %d, first %d, unchanged %d, new image %d",
			empty.ChangeToken, first.ChangeToken, again.ChangeToken, changed.ChangeToken)
	}
	// A representation that cannot be read is left out of the digest.
	if err := os.Remove(filepath.Join(f.dir, "data", key("image/png"))); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Stat(ctx); err != nil || len(st.Formats) != 2 {
		t.Errorf("stat with a vanished image: %+v, %v", st, err)
	}
}

// A clipboard the tools cannot read is empty rather than broken: wl-paste
// exits non-zero for an empty clipboard, which is the common case.
func TestAFailingToolReadsAsEmpty(t *testing.T) {
	f := installFake(t)
	c := toolClipboard(t)
	f.offer(t, nil, map[string]string{"UTF8_STRING": "x"})
	f.fail(t)
	st, err := c.Stat(context.Background())
	if err != nil || len(st.Formats) != 0 {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	if _, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("x")},
	}); err == nil {
		t.Error("a failed copy reported success")
	}
}

// A tool that cannot be run at all is a failure, not an empty clipboard.
func TestAToolThatCannotRunIsAnError(t *testing.T) {
	installFake(t)
	c := toolClipboard(t)
	c.m.(*tool).cmd = filepath.Join(t.TempDir(), "absent")
	ctx := context.Background()
	if _, err := c.Stat(ctx); err == nil {
		t.Error("stat succeeded")
	}
	if _, err := c.Read(ctx, nil, 1); err == nil {
		t.Error("read succeeded")
	}
	// The copy itself runs, but the token afterwards cannot be read.
	if _, err := c.Write(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("x")},
	}); err == nil {
		t.Error("write succeeded")
	}
}

func TestWriteRefusesAFileWithNoName(t *testing.T) {
	c := withMech(&memMech{label: "x11"})
	_, err := c.Write(context.Background(), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "..", Data: []byte("x")},
	})
	if !errors.Is(err, errBadName) {
		t.Fatalf("err = %v, want errBadName", err)
	}
}
