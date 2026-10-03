package modulesdk

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/werror"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestIdentityClient(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	ctx := ctxT(t)

	id, err := hc.Identity().WhoAmI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id != (DeviceIdentity{DeviceID: "dev-1", Ephemeral: true, Tenant: "acme"}) {
		t.Fatalf("WhoAmI = %+v", id)
	}
	cred, err := hc.Identity().Credential(ctx, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Token != "tok" || !cred.ExpiresAt.Equal(time.Unix(4102444800, 0)) ||
		!slices.Equal(cred.Scopes, []string{"read"}) {
		t.Fatalf("Credential = %+v", cred)
	}
	h.locked(func() {
		if h.badToken != 0 {
			t.Errorf("%d calls arrived without the handshake token", h.badToken)
		}
	})

	h.setFail(codes.PermissionDenied)
	if _, err := hc.Identity().WhoAmI(ctx); !errors.Is(err, werror.ErrDenied) {
		t.Fatalf("WhoAmI err = %v, want ErrDenied", err)
	}
	if _, err := hc.Identity().Credential(ctx, nil); !errors.Is(err, werror.ErrDenied) {
		t.Fatalf("Credential err = %v, want ErrDenied", err)
	}
}

func TestTransportClient(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	ctx := ctxT(t)

	delivered, err := hc.Transport().
		Send(ctx, Message{Peer: PeerHypervisor, Kind: "k", Data: []byte("d")}, false)
	if err != nil || !delivered {
		t.Fatalf("Send = %v, %v", delivered, err)
	}
	if delivered, _ := hc.Transport().Send(ctx, Message{Peer: PeerHypervisor}, true); delivered {
		t.Fatal("queued send reported delivered")
	}
	h.locked(func() {
		if got := h.sends[0]; got.GetMessage().GetKind() != "k" ||
			int(got.GetMessage().GetPeer()) != int(PeerHypervisor) {
			t.Errorf("recorded send = %v", got)
		}
	})

	ch, err := hc.Transport().Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for m := range ch {
		kinds = append(kinds, m.Kind)
	}
	if !slices.Equal(kinds, []string{"a", "b"}) {
		t.Fatalf("received %v", kinds)
	}

	h.setFail(codes.Unavailable)
	if _, err := hc.Transport().
		Send(ctx, Message{}, false); !errors.Is(
		err,
		werror.ErrUnavailable,
	) {
		t.Fatalf("Send err = %v, want ErrUnavailable", err)
	}
}

func TestPolicyClient(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	ctx := ctxT(t)

	doc, err := hc.Policy().Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Revision != 1 || doc.SchemaVersion != 2 || doc.ContentType != "application/json" ||
		string(doc.Data) != `{"x":1}` {
		t.Fatalf("Get = %+v", doc)
	}

	wctx, cancel := context.WithCancel(ctx)
	ch, err := hc.Policy().Watch(wctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := <-ch; d.Revision != 1 {
		t.Fatalf("first watched revision = %d", d.Revision)
	}
	// Revision 2 is on its way; cancelling while nobody reads must still
	// close the channel rather than leak the goroutine.
	time.Sleep(50 * time.Millisecond)
	cancel()
	for range ch {
	}

	h.setFail(codes.NotFound)
	if _, err := hc.Policy().Get(ctx); !errors.Is(err, werror.ErrNotFound) {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
}

func TestStoreClientNamespaces(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	ctx := ctxT(t)

	root, ns := hc.Store(""), hc.Store("cache")
	if err := root.Put(ctx, "top", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := ns.Put(ctx, "inner", []byte("2")); err != nil {
		t.Fatal(err)
	}
	h.locked(func() {
		if _, ok := h.store["cache/inner"]; !ok {
			t.Errorf("namespaced key not prefixed: %v", h.store)
		}
	})
	v, found, err := ns.Get(ctx, "inner")
	if err != nil || !found || string(v) != "2" {
		t.Fatalf("Get = %q, %v, %v", v, found, err)
	}
	keys, err := ns.List(ctx, "")
	if err != nil || !slices.Equal(keys, []string{"inner"}) {
		t.Fatalf("namespaced List = %v, %v", keys, err)
	}
	keys, err = root.List(ctx, "to")
	if err != nil || !slices.Equal(keys, []string{"top"}) {
		t.Fatalf("root List = %v, %v", keys, err)
	}
	if err := ns.Delete(ctx, "inner"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := ns.Get(ctx, "inner"); found {
		t.Fatal("deleted key still found")
	}

	h.setFail(codes.FailedPrecondition)
	if _, _, err := root.Get(ctx, "top"); !errors.Is(err, werror.ErrProtocol) {
		t.Fatalf("Get err = %v, want ErrProtocol", err)
	}
	if _, err := root.List(ctx, ""); err == nil {
		t.Fatal("List succeeded on a failing host")
	}
}

func TestEventsClient(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	ctx := ctxT(t)

	if err := hc.Events().Publish(ctx, "inventory", []byte("x")); err != nil {
		t.Fatal(err)
	}
	h.locked(func() {
		if h.publish[0].GetTopic() != "inventory" {
			t.Errorf("published %v", h.publish)
		}
	})
	ch, err := hc.Events().Subscribe(ctx, "a.one", "b.two")
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 2 || got[1].Topic != "b.two" || got[1].Sequence != 2 ||
		!got[0].PublishedAt.Equal(time.UnixMilli(1000)) {
		t.Fatalf("events = %+v", got)
	}
}

// A closed connection fails stream setup itself, which is the error path the
// streaming helpers return before spawning their goroutines.
func TestStreamsFailOnClosedConnection(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	hc.Close()
	ctx := ctxT(t)
	if _, err := hc.Transport().Receive(ctx); err == nil {
		t.Error("Receive succeeded on a closed connection")
	}
	if _, err := hc.Policy().Watch(ctx); err == nil {
		t.Error("Watch succeeded on a closed connection")
	}
	if _, err := hc.Events().Subscribe(ctx, "t"); err == nil {
		t.Error("Subscribe succeeded on a closed connection")
	}
}

func TestStreamGoroutinesStopOnCancel(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	for name, open := range map[string]func(context.Context) (func(), error){
		"receive": func(ctx context.Context) (func(), error) {
			ch, err := hc.Transport().Receive(ctx)
			return func() {
				for range ch {
				}
			}, err
		},
		"subscribe": func(ctx context.Context) (func(), error) {
			ch, err := hc.Events().Subscribe(ctx, "t")
			return func() {
				for range ch {
				}
			}, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(ctxT(t))
			drain, err := open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			cancel()
			drain()
		})
	}
}

func TestUIDeclareOnlyBeforeStart(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	if err := hc.UI().
		Declare(Surface{ID: "a", Title: "A", Kind: "card", Data: []byte("d")}, Surface{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	got := hc.declaredSurfaces()
	if len(got) != 2 || got[0].GetId() != "a" || got[0].GetKind() != "card" ||
		string(got[0].GetData()) != "d" {
		t.Fatalf("declared = %v", got)
	}
	hc.startJobs(context.Background())
	defer hc.stopJobs()
	if err := hc.UI().Declare(Surface{ID: "late"}); !errors.Is(err, ErrSurfacesAfterInit) {
		t.Fatalf("Declare after start: err = %v, want ErrSurfacesAfterInit", err)
	}
}

func TestLogFallsBackToLocalLogger(t *testing.T) {
	l := discardLog()
	if got := (&hostClient{log: l}).Log(); got != l {
		t.Fatal("Log without a streamed logger did not return the local one")
	}
	h := newFakeHost(t)
	hc := h.dial(t)
	hc.Log().Info("over the wire", "k", "v")
	select {
	case rec := <-h.logs:
		if rec.GetMessage() != "over the wire" || rec.GetAttrs()["k"] != "v" {
			t.Fatalf("record = %v", rec)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("log record never reached the host")
	}
}

func TestJobsRunOnScheduleAndStop(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)

	var fixed, dynamic, once atomic.Int32
	hc.addJobs([]Job{
		{Name: "fixed", Every: 5 * time.Millisecond, Run: func(context.Context) { fixed.Add(1) }},
		// EveryFunc wins over Every, and a non-positive interval ends the
		// job after the run at Start.
		{
			Name:      "once",
			Every:     time.Millisecond,
			EveryFunc: func() time.Duration { return 0 },
			Run:       func(context.Context) { once.Add(1) },
		},
		{
			Name:      "dynamic",
			EveryFunc: func() time.Duration { return 5 * time.Millisecond },
			Run:       func(context.Context) { dynamic.Add(1) },
		},
	})
	hc.stopJobs() // stopping before start is a no-op
	hc.startJobs(context.Background())
	hc.startJobs(context.Background()) // second start is a no-op
	deadline := time.Now().Add(10 * time.Second)
	for (fixed.Load() < 3 || dynamic.Load() < 3) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	hc.stopJobs()
	if fixed.Load() < 3 || dynamic.Load() < 3 {
		t.Fatalf("jobs ran fixed=%d dynamic=%d, want >=3 each", fixed.Load(), dynamic.Load())
	}
	if once.Load() != 1 {
		t.Fatalf("zero-interval job ran %d times, want 1", once.Load())
	}
	after := fixed.Load()
	time.Sleep(30 * time.Millisecond)
	if fixed.Load() != after {
		t.Fatal("job kept running after stopJobs")
	}
}

func TestWatchdogPingsAndRecovers(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	hc.setWatchdogInterval(5 * time.Millisecond)
	hc.startJobs(context.Background())

	var last uint64
	for range 3 {
		select {
		case p := <-h.pings:
			if p.GetSequence() <= last {
				t.Fatalf("sequence went %d -> %d", last, p.GetSequence())
			}
			last = p.GetSequence()
		case <-time.After(10 * time.Second):
			t.Fatal("no watchdog ping")
		}
	}
	hc.stopJobs()
}

// A stream the host ends is reopened on a later tick, and a host that cannot
// be reached at all is retried rather than ending liveness reporting.
func TestWatchdogReopensBrokenStream(t *testing.T) {
	h := newFakeHost(t)
	h.locked(func() { h.notifyFails = true })
	hc := h.dial(t)
	hc.setWatchdogInterval(2 * time.Millisecond)
	hc.startJobs(context.Background())
	time.Sleep(100 * time.Millisecond)

	h.locked(func() { h.notifyFails = false })
	select {
	case <-h.pings:
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog never recovered after the stream broke")
	}

	h.srv.Stop()
	time.Sleep(100 * time.Millisecond)
	hc.stopJobs()
}

func TestAwaitDisconnect(t *testing.T) {
	t.Run("core exits", func(t *testing.T) {
		h := newFakeHost(t)
		hc := h.dial(t)
		if _, err := hc.Identity().WhoAmI(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		lost := make(chan struct{})
		go hc.awaitDisconnect(context.Background(), func() { close(lost) })
		h.srv.Stop()
		select {
		case <-lost:
		case <-time.After(15 * time.Second):
			t.Fatal("loss of core never reported")
		}
	})
	t.Run("connection closed", func(t *testing.T) {
		h := newFakeHost(t)
		hc := h.dial(t)
		hc.Close()
		lost := make(chan struct{})
		go hc.awaitDisconnect(context.Background(), func() { close(lost) })
		select {
		case <-lost:
		case <-time.After(15 * time.Second):
			t.Fatal("closed connection never reported")
		}
	})
}

// The watch channel closes when the host goes away mid-stream, so a module
// ranging over it is not left blocked forever.
func TestPolicyWatchClosesWhenHostGoes(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	ch, err := hc.Policy().Watch(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []uint64{1, 2} {
		if d := <-ch; d.Revision != want {
			t.Fatalf("revision %d, want %d", d.Revision, want)
		}
	}
	h.srv.Stop()
	for range ch {
	}
}
