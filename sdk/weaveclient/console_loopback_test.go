package weaveclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveagent"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavedisplay"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavesession"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// The console capabilities across the real framing: clipboard content inline
// and streamed both ways, session and display, and what a host sees when the
// console session has nobody in it and core has no module to route to.

func wireOpts(
	t *testing.T,
	opts weaveclient.Options,
	svcs ...weavemodule.Service,
) *weaveclient.Client {
	t.Helper()
	client, _ := wireCore(t, opts, false, svcs...)
	return client
}

// wireCore is wireOpts returning the core too, for a test that changes its
// registry; legacy makes it a core from before weave-agent v0.9.2.
func wireCore(
	t *testing.T,
	opts weaveclient.Options,
	legacy bool,
	svcs ...weavemodule.Service,
) (*weaveclient.Client, *weavemoduletest.Core) {
	t.Helper()
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, nil)
	core.Legacy = legacy
	for _, svc := range svcs {
		core.Serve(t, svc)
	}
	go core.Run()
	opts.Log = quietLog()
	ctx, cancel := context.WithCancel(context.Background())
	client := weaveclient.New(ctx, hostConn, opts)
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		_ = core.Close()
	})
	return client, core
}

func timeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// clipboard is an in-memory guest clipboard.
type clipboard struct {
	mu    sync.Mutex
	token uint64
	items []weavewire.ClipboardItem
}

func (c *clipboard) Stat(context.Context) (weavewire.ClipboardStatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := weavewire.ClipboardStatResponse{ChangeToken: c.token}
	for _, it := range c.items {
		st.Formats = append(
			st.Formats,
			weavewire.ClipboardFormatInfo{Format: it.Format, Size: it.Size},
		)
	}
	return st, nil
}

func (c *clipboard) Read(
	_ context.Context,
	formats []weavewire.ClipboardFormat,
	_ int64,
) (weaveclipboard.Contents, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := weaveclipboard.Contents{ChangeToken: c.token}
	for _, it := range c.items {
		if len(formats) == 0 || slices.Contains(formats, it.Format) {
			out.Items = append(out.Items, it)
		}
	}
	return out, nil
}

func (c *clipboard) Write(
	_ context.Context,
	items []weavewire.ClipboardItem,
) (weavewire.ClipboardSetResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token++
	c.items = slices.Clone(items)
	res := weavewire.ClipboardSetResponse{ChangeToken: c.token}
	for _, it := range items {
		res.Written = append(res.Written, it.Format)
	}
	return res, nil
}

func TestClipboardRoundTripInline(t *testing.T) {
	client := wire(t, weaveclipboard.NewService(&clipboard{}))
	ctx := timeout(t)

	set, err := client.ClipboardSet(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("hello")},
		{Format: weavewire.ClipboardHTML, Data: []byte("<b>hello</b>")},
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ClipboardStat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The host's own write is recognisable by its token.
	if st.ChangeToken != set.ChangeToken || len(st.Formats) != 2 {
		t.Fatalf("stat = %+v after set %+v", st, set)
	}
	got, err := client.ClipboardGet(ctx, weavewire.ClipboardGetRequest{
		Formats: []weavewire.ClipboardFormat{weavewire.ClipboardHTML},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Streamed || len(got.Items) != 1 || string(got.Items[0].Data) != "<b>hello</b>" {
		t.Fatalf("get = %+v", got)
	}
}

// Content over the inline limit uploads and downloads as chunk streams, and
// the caller never sees the difference.
func TestClipboardRoundTripStreamed(t *testing.T) {
	client := wire(t, weaveclipboard.NewService(&clipboard{}))
	ctx := timeout(t)

	png := bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, weavewire.ClipboardInlineBytes/2)
	files := []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardFiles, Name: "a.txt", Data: []byte("first file")},
		{Format: weavewire.ClipboardFiles, Name: "b.bin", Data: bytes.Repeat([]byte{9}, 70_000)},
	}
	if _, err := client.ClipboardSet(ctx, append([]weavewire.ClipboardItem{
		{Format: weavewire.ClipboardPNG, Data: png},
	}, files...)); err != nil {
		t.Fatal(err)
	}
	got, err := client.ClipboardGet(ctx, weavewire.ClipboardGetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Streamed || len(got.Items) != 3 {
		t.Fatalf("get = streamed %v, %d items", got.Streamed, len(got.Items))
	}
	if !bytes.Equal(got.Items[0].Data, png) || got.Items[1].Name != "a.txt" ||
		string(
			got.Items[1].Data,
		) != "first file" || !bytes.Equal(got.Items[2].Data, files[1].Data) {
		t.Fatal("streamed content came back different")
	}
}

func TestClipboardSetRefusesWhatCannotFit(t *testing.T) {
	client := wire(t)
	_, err := client.ClipboardSet(timeout(t), []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardPNG, Data: make([]byte, weavewire.MaxClipboardBytes+1)},
	})
	if !errors.Is(err, weaveclient.ErrTransfer) {
		t.Fatalf("err = %v", err)
	}
}

// With nobody at the console core has no clipboard or display module to route
// to and drops the command. The host gets a clear, matchable error instead
// of a hang — and session, which runs as system, still answers.
//
// This is a core from before weave-agent v0.9.2, which says nothing about a
// frame it cannot deliver: silence and the session timeout are all there is.
// TestConsoleCapabilitiesWaitingForSession is the same guest under a core
// that says why.
func TestConsoleCapabilitiesWithNobodyLoggedIn(t *testing.T) {
	client, _ := wireCore(t, weaveclient.Options{SessionTimeout: 50 * time.Millisecond}, true,
		weavesession.NewService(&consoleSession{}))
	ctx := timeout(t)

	calls := map[string]func() error{
		"clipboard stat": func() error { _, err := client.ClipboardStat(ctx); return err },
		"clipboard get": func() error {
			_, err := client.ClipboardGet(ctx, weavewire.ClipboardGetRequest{})
			return err
		},
		"clipboard set": func() error {
			_, err := client.ClipboardSet(ctx, []weavewire.ClipboardItem{
				{
					Format: weavewire.ClipboardText,
					Data:   bytes.Repeat([]byte("x"), weavewire.ClipboardInlineBytes+1),
				},
			})
			return err
		},
		"display list": func() error { _, err := client.DisplayList(ctx); return err },
		"display set": func() error {
			_, err := client.DisplaySet(ctx, weavewire.DisplaySetRequest{Width: 800, Height: 600})
			return err
		},
	}
	for name, call := range calls {
		start := time.Now()
		err := call()
		if !errors.Is(err, weaveclient.ErrNoSession) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: err = %v", name, err)
		}
		if waited := time.Since(start); waited > 5*time.Second {
			t.Errorf("%s waited %s", name, waited)
		}
	}

	cur, err := client.SessionCurrent(ctx)
	if err != nil || cur != nil {
		t.Fatalf("session current = %+v, %v", cur, err)
	}
}

// The caller's own deadline on a console call means the same thing; a
// cancellation is the caller's and stays one; system capabilities are not
// touched.
//
// A core from before weave-agent v0.9.2, so silence is all there is.
func TestNoSessionAndTheCallersContext(t *testing.T) {
	client, _ := wireCore(t, weaveclient.Options{SessionTimeout: -1}, true)

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.ClipboardStat(short); !errors.Is(err, weaveclient.ErrNoSession) {
		t.Fatalf("err = %v", err)
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := client.ClipboardStat(cancelled); !errors.Is(err, context.Canceled) ||
		errors.Is(err, weaveclient.ErrNoSession) {
		t.Fatalf("err = %v", err)
	}

	// No time module either, but time is a system capability: silence there
	// is a missing agent, not a missing login.
	short2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := client.Time(short2); !errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, weaveclient.ErrNoSession) {
		t.Fatalf("err = %v", err)
	}
	// Session runs as system too, so its silence is no-agent as well.
	if cur, err := client.SessionCurrent(short2); !errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, weaveclient.ErrNoSession) || cur != nil {
		t.Fatalf("current = %+v, err = %v", cur, err)
	}
}

// consoleSession is a session backend whose console the test changes.
type consoleSession struct {
	mu     sync.Mutex
	cur    *weavewire.SessionInfo
	locked []string
}

func (s *consoleSession) set(cur *weavewire.SessionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = cur
}

func (s *consoleSession) Console(context.Context) (weavewire.SessionInfo, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return weavewire.SessionInfo{}, false, nil
	}
	return *s.cur, true, nil
}

func (s *consoleSession) List(context.Context) ([]weavewire.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil, nil
	}
	return []weavewire.SessionInfo{*s.cur}, nil
}

func (s *consoleSession) Lock(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locked = append(s.locked, id)
	return nil
}

func TestSessionAcrossTheRealFraming(t *testing.T) {
	b := &consoleSession{
		cur: &weavewire.SessionInfo{ID: "1", User: "alice", State: weavewire.SessionActive},
	}
	client := wire(t, weavesession.NewService(b, weavesession.WithPollInterval(5*time.Millisecond)))
	ctx := timeout(t)

	changed := make(chan weavewire.SessionChangedEvent, 4)
	client.OnSessionChanged(func(ev weavewire.SessionChangedEvent) { changed <- ev })

	cur, err := client.SessionCurrent(ctx)
	if err != nil || cur == nil || cur.User != "alice" {
		t.Fatalf("current = %+v, %v", cur, err)
	}
	list, err := client.SessionList(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	id, err := client.SessionLock(ctx, "")
	if err != nil || id != "1" {
		t.Fatalf("lock = %q, %v", id, err)
	}

	b.set(nil)
	select {
	case ev := <-changed:
		if ev.Previous == nil || ev.Previous.User != "alice" || ev.Current != nil {
			t.Fatalf("event = %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("no session change event")
	}
}

func TestSessionOpsAnUnequippedOSCannotDo(t *testing.T) {
	client := wire(t, weavesession.NewService(onlyConsole{}))
	if _, err := client.SessionList(timeout(t)); !errors.Is(err, weaveclient.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

type onlyConsole struct{}

func (onlyConsole) Console(context.Context) (weavewire.SessionInfo, bool, error) {
	return weavewire.SessionInfo{}, false, nil
}

type displays struct{ current weavewire.DisplayMode }

func (d *displays) List(context.Context) ([]weavewire.DisplayInfo, error) {
	return []weavewire.DisplayInfo{{ID: "Virtual-1", Primary: true, Current: d.current}}, nil
}

func (d *displays) Set(
	_ context.Context,
	req weavewire.DisplaySetRequest,
) (weavewire.DisplayInfo, error) {
	d.current = weavewire.DisplayMode{Width: req.Width, Height: req.Height}
	return weavewire.DisplayInfo{ID: req.DisplayID, Primary: true, Current: d.current}, nil
}

func TestDisplayAcrossTheRealFraming(t *testing.T) {
	client := wire(
		t,
		weavedisplay.NewService(
			&displays{current: weavewire.DisplayMode{Width: 1024, Height: 768}},
		),
	)
	ctx := timeout(t)
	got, err := client.DisplaySet(ctx, weavewire.DisplaySetRequest{Width: 1440, Height: 900})
	if err != nil || got.ID != "Virtual-1" || got.Current.Width != 1440 {
		t.Fatalf("set = %+v, %v", got, err)
	}
	list, err := client.DisplayList(ctx)
	if err != nil || len(list) != 1 || list[0].Current.Height != 900 {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// rogueClipboard answers a get with a manifest and then misbehaves on the
// stream, the way a module killed by a logout or a broken channel would.
type rogueClipboard struct {
	size   int64
	stream func(ctx context.Context, w *weaveagent.StreamWriter, emit weaveagent.Emitter, id string)
}

func (r *rogueClipboard) Capability() weavewire.Capability { return weavewire.Clipboard }

func (r *rogueClipboard) Register(reg *weavemodule.Registrar) error {
	emit := reg.Emitter()
	reg.HandleDeferred(
		weavewire.KindClipboardGet,
		func(ctx context.Context, payload []byte) ([]byte, func(), error) {
			var req weavewire.ClipboardGetRequest
			_ = json.Unmarshal(payload, &req)
			out, err := weavewire.EncodePayload(weavewire.ClipboardGetResponse{
				Streamed: true,
				Items:    []weavewire.ClipboardItem{{Format: weavewire.ClipboardPNG, Size: r.size}},
			})
			return out, func() {
				r.stream(
					ctx,
					weaveagent.NewStreamWriter(
						emit,
						weavewire.KindClipboardDownload,
						req.TransferID,
					),
					emit,
					req.TransferID,
				)
			}, err
		},
	)
	return nil
}

func TestClipboardGetStreamsThatGoWrong(t *testing.T) {
	for name, tc := range map[string]struct {
		size   int64
		stream func(ctx context.Context, w *weaveagent.StreamWriter, emit weaveagent.Emitter, id string)
		noSess bool
	}{
		"stalls": {10, func(ctx context.Context, w *weaveagent.StreamWriter, _ weaveagent.Emitter, _ string) {
			_, _ = w.WriteContext(ctx, []byte("half"))
		}, true},
		"fails": {10, func(ctx context.Context, w *weaveagent.StreamWriter, _ weaveagent.Emitter, _ string) {
			_, _ = w.WriteContext(ctx, []byte("half"))
			_ = w.Close(ctx, errors.New("pasteboard went away"))
		}, false},
		"short": {10, func(ctx context.Context, w *weaveagent.StreamWriter, _ weaveagent.Emitter, _ string) {
			_, _ = w.WriteContext(ctx, []byte("half"))
			_ = w.Close(ctx, nil)
		}, false},
		"gap": {10, func(ctx context.Context, _ *weaveagent.StreamWriter, emit weaveagent.Emitter, id string) {
			_ = emit.Emit(ctx, weavewire.KindClipboardDownload, "not a chunk")
			_ = emit.Emit(ctx, weavewire.KindClipboardDownload, weavewire.Chunk{StreamID: "someone-else", Seq: 0})
			_ = emit.Emit(ctx, weavewire.KindClipboardDownload, weavewire.Chunk{StreamID: id, Seq: 3, Data: []byte("x")})
			_ = emit.Emit(ctx, weavewire.KindClipboardDownload, weavewire.Chunk{StreamID: id, Seq: 4, EOF: true})
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			client := wireOpts(t, weaveclient.Options{SessionTimeout: 200 * time.Millisecond},
				&rogueClipboard{size: tc.size, stream: tc.stream})
			_, err := client.ClipboardGet(timeout(t), weavewire.ClipboardGetRequest{})
			if !errors.Is(err, weaveclient.ErrTransfer) {
				t.Fatalf("err = %v", err)
			}
			if errors.Is(err, weaveclient.ErrNoSession) != tc.noSess {
				t.Fatalf("err = %v, want no-session %v", err, tc.noSess)
			}
		})
	}
}

// The download waits on the caller and on the channel as well as the stream.
func TestClipboardGetStreamEndsWithTheCallerOrTheChannel(t *testing.T) {
	never := func(context.Context, *weaveagent.StreamWriter, weaveagent.Emitter, string) {}

	client := wireOpts(
		t,
		weaveclient.Options{SessionTimeout: -1},
		&rogueClipboard{size: 1, stream: never},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := client.ClipboardGet(
		ctx,
		weavewire.ClipboardGetRequest{},
	); !errors.Is(
		err,
		context.DeadlineExceeded,
	) {
		t.Fatalf("err = %v", err)
	}

	client = wireOpts(t, weaveclient.Options{}, &rogueClipboard{size: 1, stream: never})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = client.Close()
	}()
	if _, err := client.ClipboardGet(
		timeout(t),
		weavewire.ClipboardGetRequest{},
	); !errors.Is(
		err,
		weaveclient.ErrClosed,
	) {
		t.Fatalf("err = %v", err)
	}
}

func TestUndecodableSessionChangesAreDropped(t *testing.T) {
	sent := make(chan struct{})
	client := wire(t, &emitOnce{kind: weavewire.KindSessionChanged, payload: "garbage", sent: sent})
	got := make(chan weavewire.SessionChangedEvent, 1)
	client.OnSessionChanged(func(ev weavewire.SessionChangedEvent) { got <- ev })
	// Hello makes the channel live; the emitter fires on its first command.
	if _, err := client.SessionCurrent(timeout(t)); err != nil {
		t.Fatal(err)
	}
	<-sent
	if _, err := client.Hello(timeout(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-got:
		t.Fatalf("garbage was delivered as %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// emitOnce is a session module that answers current and emits one raw event.
type emitOnce struct {
	kind    string
	payload any
	sent    chan struct{}
}

func (e *emitOnce) Capability() weavewire.Capability { return weavewire.Session }

func (e *emitOnce) Register(r *weavemodule.Registrar) error {
	emit := r.Emitter()
	r.HandleDeferred(
		weavewire.KindSessionCurrent,
		func(ctx context.Context, _ []byte) ([]byte, func(), error) {
			out, err := weavewire.EncodePayload(weavewire.SessionCurrentResponse{})
			return out, func() {
				_ = emit.Emit(ctx, e.kind, e.payload)
				close(e.sent)
			}, err
		},
	)
	return nil
}
