package weaveclient_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavepresence"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavetime"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// The module registry and delivery.failed, against weavemoduletest.Core
// (which speaks them as weave-agent v0.9.2 does) and against a hand-driven
// guest for the orderings a real core only produces under load.

// quick is how long an answer core gives at once may take: far less than
// any timeout the client would otherwise wait out.
const quick = 2 * time.Second

func wireTrusted(
	t *testing.T,
	trusted ed25519.PublicKey,
	opts weaveclient.Options,
	svcs ...weavemodule.Service,
) (*weaveclient.Client, *weavemoduletest.Core) {
	t.Helper()
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, trusted)
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

// Each reason core gives fails the call it was about at once, as an error
// that says which.
func TestDeliveryFailureReasons(t *testing.T) {
	client, core := wireCore(t, weaveclient.Options{}, false,
		weavepresence.NewService(fakeKernel{}, nil), weavetime.NewService(nopClock{}))
	core.SetModule(hvchannel.ModuleInfo{
		ID: "weave-test-clipboard", Address: "weave.clipboard",
		State: weaveclient.ModuleStateWaitingForSession, Detail: "no console user session",
	})
	core.SetBusy("weave.time", true)
	ctx := timeout(t)

	cases := []struct {
		name   string
		call   func() error
		want   error
		reason string
		state  string
	}{
		{
			name:   "not installed",
			call:   func() error { _, err := client.Shutdown(ctx, "test"); return err },
			want:   weaveclient.ErrModuleNotInstalled,
			reason: hvchannel.ReasonNotInstalled,
		},
		{
			name:   "not running",
			call:   func() error { _, err := client.ClipboardStat(ctx); return err },
			want:   weaveclient.ErrModuleNotRunning,
			reason: hvchannel.ReasonNotRunning,
			state:  weaveclient.ModuleStateWaitingForSession,
		},
		{
			name:   "busy",
			call:   func() error { _, err := client.Time(ctx); return err },
			want:   weaveclient.ErrModuleBusy,
			reason: hvchannel.ReasonBusy,
		},
		{
			name: "exec not installed",
			call: func() error {
				_, err := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"true"}})
				return err
			},
			want:   weaveclient.ErrModuleNotInstalled,
			reason: hvchannel.ReasonNotInstalled,
		},
	}
	sentinels := []error{
		weaveclient.ErrModuleNotInstalled,
		weaveclient.ErrModuleNotRunning,
		weaveclient.ErrModuleBusy,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			err := c.call()
			if waited := time.Since(start); waited > quick {
				t.Errorf("failed after %s, not at once", waited)
			}
			if !errors.Is(err, c.want) || !errors.Is(err, weaveclient.ErrUndeliverable) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			for _, other := range sentinels {
				if other != c.want && errors.Is(err, other) {
					t.Errorf("err %v also matches %v", err, other)
				}
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err %v reads as a timeout", err)
			}
			var de *weaveclient.DeliveryError
			if !errors.As(err, &de) || de.Reason != c.reason || de.State != c.state ||
				de.Module == "" || !strings.HasPrefix(de.Kind, de.Module+".") {
				t.Fatalf("err = %#v", err)
			}
		})
	}

	// A console capability waiting for a login is still ErrNoSession — now
	// at once rather than after the session timeout.
	_, err := client.ClipboardStat(ctx)
	if !errors.Is(err, weaveclient.ErrNoSession) {
		t.Fatalf("waiting-for-session: err = %v, want ErrNoSession too", err)
	}
	var de *weaveclient.DeliveryError
	if errors.As(err, &de); !strings.Contains(de.Error(), "no console user session") ||
		!strings.Contains(de.Error(), "waiting-for-session") {
		t.Fatalf("message %q", de.Error())
	}

	// The channel is unharmed, and the call that core can deliver works.
	if _, err := client.Hello(ctx); err != nil {
		t.Fatalf("hello after failures: %v", err)
	}
}

// TestConsoleCapabilitiesWithNobodyLoggedIn under a core that says why:
// each console call fails as ErrNoSession at once, not after the timeout.
func TestConsoleCapabilitiesWaitingForSession(t *testing.T) {
	client, core := wireCore(t, weaveclient.Options{SessionTimeout: time.Minute}, false)
	for _, addr := range []string{"weave.clipboard", "weave.display"} {
		core.SetModule(hvchannel.ModuleInfo{
			ID: "test-" + addr, Address: addr, State: weaveclient.ModuleStateWaitingForSession,
		})
	}
	ctx := timeout(t)
	for name, call := range map[string]func() error{
		"clipboard stat": func() error { _, err := client.ClipboardStat(ctx); return err },
		"display list":   func() error { _, err := client.DisplayList(ctx); return err },
	} {
		start := time.Now()
		if err := call(); !errors.Is(err, weaveclient.ErrNoSession) ||
			!errors.Is(err, weaveclient.ErrModuleNotRunning) {
			t.Errorf("%s: err = %v", name, err)
		}
		if waited := time.Since(start); waited > quick {
			t.Errorf("%s waited %s", name, waited)
		}
	}
}

// Core's answer reaches the call it names and no other: a delivery.failed
// for one of two calls in flight fails that one, and the other still gets
// its reply.
func TestDeliveryFailedFailsOnlyTheNamedCall(t *testing.T) {
	guest, client := newRaw(t)
	ctx := timeout(t)

	type result struct {
		which string
		err   error
	}
	results := make(chan result, 2)
	go func() { _, err := client.Time(ctx); results <- result{"time", err} }()
	first := guest.read()
	go func() { _, err := client.Metrics(ctx); results <- result{"metrics", err} }()
	second := guest.read()

	var cmd weavewire.Command
	_ = json.Unmarshal(second.Data, &cmd)
	if second.ID == "" || second.ID != cmd.ID {
		t.Fatalf("envelope id %q, command id %q: a call's envelope must carry its command id",
			second.ID, cmd.ID)
	}
	failed, _ := json.Marshal(hvchannel.DeliveryFailed{
		Module: second.Module, Kind: second.Kind, Reason: hvchannel.ReasonNotInstalled,
	})
	guest.write(hvchannel.Envelope{
		Module: hvchannel.ControlModule,
		Kind:   hvchannel.KindDeliveryFailed,
		Data:   failed,
		ID:     second.ID,
	})
	if r := <-results; r.which != "metrics" ||
		!errors.Is(r.err, weaveclient.ErrModuleNotInstalled) {
		t.Fatalf("%s: %v", r.which, r.err)
	}
	guest.reply(first, weavewire.Result{Payload: json.RawMessage(`{}`)})
	if r := <-results; r.which != "time" || r.err != nil {
		t.Fatalf("%s: %v", r.which, r.err)
	}

	// Frames nobody is waiting for — no id, an id that has gone, a body
	// that will not decode — are dropped, and the channel lives on.
	for _, env := range []hvchannel.Envelope{
		{Module: hvchannel.ControlModule, Kind: hvchannel.KindDeliveryFailed, Data: failed},
		{Module: hvchannel.ControlModule, Kind: hvchannel.KindDeliveryFailed, Data: failed, ID: second.ID},
		{Module: hvchannel.ControlModule, Kind: hvchannel.KindDeliveryFailed, Data: []byte(`[]`)},
		{Module: hvchannel.ControlModule, Kind: hvchannel.KindAuthResult, Data: []byte(`{}`), ID: "gone"},
		{Module: hvchannel.ControlModule, Kind: hvchannel.KindAuthResult, Data: []byte(`{"ok":true}`), ID: "x"},
		{Module: hvchannel.ControlModule, Kind: "modules.someday"},
	} {
		guest.write(env)
	}
	go func() { _, err := client.Time(ctx); results <- result{"time", err} }()
	again := guest.read()

	// An undecodable delivery.failed for a waiting call still ends it.
	guest.write(hvchannel.Envelope{
		Module: hvchannel.ControlModule,
		Kind:   hvchannel.KindDeliveryFailed,
		Data:   []byte(`[]`),
		ID:     again.ID,
	})
	if r := <-results; !errors.Is(r.err, weaveclient.ErrUndeliverable) ||
		errors.Is(r.err, weaveclient.ErrModuleBusy) {
		t.Fatalf("undecodable delivery.failed: %v", r.err)
	}
	// A late reply to it after all has nobody to go to.
	guest.reply(again, weavewire.Result{Payload: json.RawMessage(`{}`)})
	if client.Err() != nil {
		t.Fatalf("channel ended: %v", client.Err())
	}
}

// A refusal that echoes an id fails that call alone; one without (a core
// from before ids) fails every call in flight, as it always did.
func TestRefusalsByID(t *testing.T) {
	guest, client := newRaw(t)
	ctx := timeout(t)
	errs := make(chan error, 2)
	go func() { _, err := client.Time(ctx); errs <- err }()
	first := guest.read()
	go func() { _, err := client.Metrics(ctx); errs <- err }()
	second := guest.read()

	refusal := []byte(`{"reason":"channel is not authenticated"}`)
	guest.write(hvchannel.Envelope{
		Module: hvchannel.ControlModule,
		Kind:   hvchannel.KindAuthResult,
		Data:   refusal,
		ID:     second.ID,
	})
	if err := <-errs; !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("refused call: %v", err)
	}
	guest.reply(first, weavewire.Result{Payload: json.RawMessage(`{}`)})
	if err := <-errs; err != nil {
		t.Fatalf("the other call: %v", err)
	}

	go func() { _, err := client.Time(ctx); errs <- err }()
	guest.read()
	go func() { _, err := client.Metrics(ctx); errs <- err }()
	guest.read()
	guest.write(
		hvchannel.Envelope{
			Module: hvchannel.ControlModule,
			Kind:   hvchannel.KindAuthResult,
			Data:   refusal,
		},
	)
	for range 2 {
		if err := <-errs; !errors.Is(err, weaveclient.ErrNotAuthenticated) {
			t.Fatalf("id-less refusal: %v", err)
		}
	}
}

// Against a v0.9.2 core: the registry, the cached view and the gates.
func TestModulesFromACoreThatAnswers(t *testing.T) {
	client, core := wireCore(t, weaveclient.Options{}, false,
		weavepresence.NewService(fakeKernel{}, nil))
	ctx := timeout(t)

	if _, ok := client.Snapshot(); ok {
		t.Fatal("a snapshot before asking")
	}
	if client.Installed("weave.presence") {
		t.Fatal("installed with no snapshot")
	}
	snap, err := client.Modules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision == 0 || len(snap.Modules) != 1 || snap.Modules[0].Address != "weave.presence" {
		t.Fatalf("snapshot %+v", snap)
	}
	if !client.Installed("weave.presence") || !client.Running("weave.presence") ||
		client.Installed("weave.power") || client.Running("weave.power") {
		t.Fatal("cached gates disagree with the snapshot")
	}
	m, ok := client.Module("weave.presence")
	if !ok || m.State != weaveclient.ModuleStateRunning ||
		m.Health.Status != hvchannel.HealthHealthy {
		t.Fatalf("Module = %+v, %v", m, ok)
	}

	changed := make(chan weaveclient.ModulesSnapshot, 4)
	client.OnModulesChanged(func(s weaveclient.ModulesSnapshot) { changed <- s })
	core.SetModule(hvchannel.ModuleInfo{
		ID: "weave-test-clipboard", Address: "weave.clipboard",
		State: weaveclient.ModuleStateWaitingForSession,
	})
	select {
	case s := <-changed:
		if s.Revision <= snap.Revision || len(s.Modules) != 2 {
			t.Fatalf("pushed %+v", s)
		}
	case <-time.After(quick):
		t.Fatal("no modules.changed reached the handler")
	}
	if !client.Installed("weave.clipboard") || client.Running("weave.clipboard") {
		t.Fatal("the push did not update the cache")
	}
	cached, ok := client.Snapshot()
	if !ok || len(cached.Modules) != 2 {
		t.Fatalf("Snapshot = %+v, %v", cached, ok)
	}
}

// Against a core from before v0.9.2, Modules ends in ErrRegistryUnsupported
// after the registry timeout, not a hang; a context shorter than that is the
// caller's own.
func TestModulesFromACoreThatDoesNotAnswer(t *testing.T) {
	client, _ := wireCore(t, weaveclient.Options{RegistryTimeout: 100 * time.Millisecond}, true,
		weavepresence.NewService(fakeKernel{}, nil))

	start := time.Now()
	_, err := client.Modules(timeout(t))
	if !errors.Is(err, weaveclient.ErrRegistryUnsupported) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrRegistryUnsupported", err)
	}
	if waited := time.Since(start); waited > quick {
		t.Fatalf("waited %s", waited)
	}
	if _, ok := client.Snapshot(); ok {
		t.Fatal("a snapshot from a core with no registry")
	}

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	slow, _ := wireCore(t, weaveclient.Options{RegistryTimeout: -1}, true)
	if _, err := slow.Modules(short); errors.Is(err, weaveclient.ErrRegistryUnsupported) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller's deadline: err = %v", err)
	}

	// The default bound applies when none is set.
	if weaveclient.DefaultRegistryTimeout <= 0 ||
		weaveclient.DefaultRegistryTimeout > 10*time.Second {
		t.Fatalf("DefaultRegistryTimeout = %s", weaveclient.DefaultRegistryTimeout)
	}

	// Everything else still works against the older core.
	if _, err := client.Hello(timeout(t)); err != nil {
		t.Fatal(err)
	}
}

// A push and a list answer race; whichever has the higher revision wins, and
// the change handler sees each revision that advanced the view, once.
func TestModulesKeepsTheHighestRevision(t *testing.T) {
	guest, client := newRaw(t)
	ctx := timeout(t)

	var mu sync.Mutex
	var seen []uint64
	advanced := make(chan struct{}, 8)
	client.OnModulesChanged(func(s weaveclient.ModulesSnapshot) {
		mu.Lock()
		seen = append(seen, s.Revision)
		mu.Unlock()
		advanced <- struct{}{}
	})
	snapshot := func(kind, id string, rev uint64) {
		data, _ := json.Marshal(
			weaveclient.ModulesSnapshot{Revision: rev, Modules: []weaveclient.ModuleInfo{
				{ID: "m", Address: "weave.m", State: weaveclient.ModuleStateRunning},
			}},
		)
		guest.write(
			hvchannel.Envelope{Module: hvchannel.ControlModule, Kind: kind, Data: data, ID: id},
		)
	}

	type answer struct {
		snap weaveclient.ModulesSnapshot
		err  error
	}
	answers := make(chan answer, 1)
	go func() { s, err := client.Modules(ctx); answers <- answer{s, err} }()
	list := guest.read()
	if list.Module != hvchannel.ControlModule || list.Kind != hvchannel.KindModulesList ||
		list.ID == "" {
		t.Fatalf("modules.list sent as %+v", list)
	}
	// The push overtakes the answer.
	snapshot(hvchannel.KindModulesChanged, "", 5)
	snapshot(hvchannel.KindModulesListResult, list.ID, 3)
	a := <-answers
	if a.err != nil || a.snap.Revision != 5 {
		t.Fatalf("Modules = rev %d, %v; want the newer push, 5", a.snap.Revision, a.err)
	}
	snapshot(hvchannel.KindModulesChanged, "", 4) // older: ignored
	snapshot(hvchannel.KindModulesChanged, "", 6)
	<-advanced
	<-advanced
	mu.Lock()
	got := append([]uint64(nil), seen...)
	mu.Unlock()
	if len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("handler saw revisions %v, want [5 6]", got)
	}

	// Without a nil module list on the wire the client still hands out an
	// empty slice; an undecodable answer fails the call that asked.
	go func() { s, err := client.Modules(ctx); answers <- answer{s, err} }()
	list = guest.read()
	guest.write(hvchannel.Envelope{
		Module: hvchannel.ControlModule,
		Kind:   hvchannel.KindModulesListResult,
		Data:   []byte(`[]`),
		ID:     list.ID,
	})
	if a := <-answers; a.err == nil {
		t.Fatal("an undecodable snapshot was accepted")
	}
	guest.write(hvchannel.Envelope{
		Module: hvchannel.ControlModule,
		Kind:   hvchannel.KindModulesChanged,
		Data:   []byte(`{"revision":9}`),
	})
	<-advanced
	if s, _ := client.Snapshot(); s.Revision != 9 || s.Modules == nil {
		t.Fatalf("snapshot %+v", s)
	}
}

// handshake answers one Authenticate by hand: the signature is not checked,
// which is not what this is about.
func (g *rawGuest) handshake() {
	g.t.Helper()
	g.read() // auth.begin
	nonce, _ := hvchannel.NewNonce()
	data, _ := json.Marshal(hvchannel.AuthChallenge{Nonce: nonce})
	g.write(
		hvchannel.Envelope{
			Module: hvchannel.ControlModule,
			Kind:   hvchannel.KindAuthChallenge,
			Data:   data,
		},
	)
	g.read() // auth.response
	g.write(hvchannel.Envelope{
		Module: hvchannel.ControlModule,
		Kind:   hvchannel.KindAuthResult,
		Data:   []byte(`{"ok":true}`),
	})
}

// Authenticating fetches the registry afresh, and drops the old view first:
// a re-authentication may be to a core that restarted, with its revisions
// back at 1. Registry frames that arrive mid-handshake are not swallowed by
// it.
func TestReauthenticationRefreshesTheRegistry(t *testing.T) {
	guest, client := newRaw(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ctx := timeout(t)
	revisions := make(chan uint64, 8)
	client.OnModulesChanged(func(s weaveclient.ModulesSnapshot) { revisions <- s.Revision })

	answerList := func(rev uint64) {
		list := guest.read()
		if list.Kind != hvchannel.KindModulesList {
			t.Errorf("after authenticating the client sent %s, want modules.list", list.Kind)
		}
		data, _ := json.Marshal(weaveclient.ModulesSnapshot{Revision: rev})
		guest.write(hvchannel.Envelope{
			Module: hvchannel.ControlModule,
			Kind:   hvchannel.KindModulesListResult,
			Data:   data,
			ID:     list.ID,
		})
	}

	first := make(chan struct{})
	go func() {
		defer close(first)
		guest.read() // auth.begin
		// A push lands mid-handshake: it is news, not the challenge.
		guest.write(hvchannel.Envelope{
			Module: hvchannel.ControlModule, Kind: hvchannel.KindModulesChanged,
			Data: []byte(`{"revision":40,"modules":[]}`),
		})
		nonce, _ := hvchannel.NewNonce()
		data, _ := json.Marshal(hvchannel.AuthChallenge{Nonce: nonce})
		guest.write(
			hvchannel.Envelope{
				Module: hvchannel.ControlModule,
				Kind:   hvchannel.KindAuthChallenge,
				Data:   data,
			},
		)
		guest.read()
		guest.write(hvchannel.Envelope{
			Module: hvchannel.ControlModule,
			Kind:   hvchannel.KindAuthResult,
			Data:   []byte(`{"ok":true}`),
		})
		answerList(10)
	}()
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatal(err)
	}
	if r := <-revisions; r != 40 {
		t.Fatalf("mid-handshake push: revision %d", r)
	}
	if r := <-revisions; r != 10 {
		t.Fatalf("after authenticating: revision %d", r)
	}

	<-first // one guest-side writer at a time

	// Core restarts behind the channel; the host authenticates again.
	go func() {
		guest.handshake()
		answerList(1)
	}()
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatal(err)
	}
	if r := <-revisions; r != 1 {
		t.Fatalf("after re-authenticating: revision %d, want the new core's 1", r)
	}
	if s, ok := client.Snapshot(); !ok || s.Revision != 1 {
		t.Fatalf("snapshot %+v", s)
	}
}

// Against weavemoduletest.Core with the authentication gate: Modules before
// authenticating is refused at once, auth replies still work, and a handler
// registered before authenticating sees the registry from the start.
func TestModulesAndAuthentication(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	client, core := wireTrusted(t, pub, weaveclient.Options{},
		weavepresence.NewService(fakeKernel{}, stubInventory{}))
	ctx := timeout(t)

	start := time.Now()
	if _, err := client.Modules(ctx); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth Modules: %v", err)
	}
	if time.Since(start) > quick {
		t.Fatal("the refusal was waited out")
	}
	if _, err := client.Inventory(ctx); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth inventory: %v", err)
	}
	// Before authenticating, a frame for a missing module goes unanswered:
	// core does not tell an unauthenticated host what it has.
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := client.Shutdown(short, "x"); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth shutdown: %v", err)
	}

	got := make(chan weaveclient.ModulesSnapshot, 4)
	client.OnModulesChanged(func(s weaveclient.ModulesSnapshot) { got <- s })
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if len(s.Modules) != 1 || s.Modules[0].Address != "weave.presence" {
			t.Fatalf("refreshed %+v", s)
		}
	case <-time.After(quick):
		t.Fatal("authenticating did not refresh the registry")
	}
	if _, err := client.Inventory(ctx); err != nil {
		t.Fatalf("inventory after authenticating: %v", err)
	}
	core.RemoveModule("nothing")
	if s := <-got; s.Revision < 2 {
		t.Fatalf("push after authenticating: %+v", s)
	}
	if _, err := client.Shutdown(ctx, "x"); !errors.Is(err, weaveclient.ErrModuleNotInstalled) {
		t.Fatalf("authenticated shutdown with no power module: %v", err)
	}
}

// A core from before v0.9.2 refuses without echoing the id, and the client
// still fails the calls in flight rather than waiting.
func TestLegacyRefusalStillFailsCallsInFlight(t *testing.T) {
	trusted, _, _ := ed25519.GenerateKey(rand.Reader)
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, trusted)
	core.Legacy = true
	core.Serve(t, weavepresence.NewService(fakeKernel{}, stubInventory{}))
	go core.Run()
	client := weaveclient.New(context.Background(), hostConn, weaveclient.Options{Log: quietLog()})
	t.Cleanup(func() { _ = client.Close(); _ = core.Close() })

	if _, err := client.Inventory(timeout(t)); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("err = %v", err)
	}
}

// An exec session whose input core cannot deliver ends with core's reason:
// Wait returns it and the output readers end with it. Busy for input sent
// unconfirmed — before the client knew core reports refusals, so it cannot
// tell which chunk was refused — only cuts off the input; the process runs on
// and its exit still arrives. (Confirmed input rides out busy:
// TestExecInputRidesOutABusyModule.)
func TestExecSessionUndeliverableInput(t *testing.T) {
	for _, c := range []struct {
		reason string
		ends   bool
		want   error
	}{
		{hvchannel.ReasonNotRunning, true, weaveclient.ErrModuleNotRunning},
		{hvchannel.ReasonNotInstalled, true, weaveclient.ErrModuleNotInstalled},
		{hvchannel.ReasonBusy, false, weaveclient.ErrModuleBusy},
	} {
		t.Run(c.reason, func(t *testing.T) {
			guest, client := newRaw(t)
			ctx := timeout(t)
			s := startExec(t, guest, client, ctx)

			wrote := make(chan error, 1)
			go func() { _, err := s.Write([]byte("input")); wrote <- err }()
			stdin := guest.read()
			if stdin.Kind != weavewire.KindExecStdin || stdin.ID != s.ID() {
				t.Fatalf(
					"stdin sent as %s with id %q, want the exec id %q",
					stdin.Kind,
					stdin.ID,
					s.ID(),
				)
			}
			if err := <-wrote; err != nil {
				t.Fatal(err)
			}
			failed, _ := json.Marshal(hvchannel.DeliveryFailed{
				Module: "weave.exec",
				Kind:   weavewire.KindExecStdin,
				Reason: c.reason,
				State:  "backoff",
			})
			guest.write(hvchannel.Envelope{
				Module: hvchannel.ControlModule,
				Kind:   hvchannel.KindDeliveryFailed,
				Data:   failed,
				ID:     s.ID(),
			})

			if !c.ends {
				// Input is refused from now on, once the read loop has seen
				// core's answer. Until then a chunk may still go out, so the
				// guest end drains whatever arrives, and answers a fence as
				// core does: the refusal also tells the client core reports
				// them, so a close racing it confirms itself with a
				// modules.list, which left unanswered would wait forever.
				go func() {
					for {
						env, err := hvchannel.ReadEnvelope(guest.r)
						if err != nil {
							return
						}
						if env.Kind != hvchannel.KindModulesList {
							continue
						}
						data, _ := json.Marshal(weaveclient.ModulesSnapshot{Revision: 1})
						guest.wmu.Lock()
						_ = hvchannel.WriteEnvelope(guest.w, hvchannel.Envelope{
							Module: hvchannel.ControlModule, Kind: hvchannel.KindModulesListResult,
							Data: data, ID: env.ID,
						})
						_ = guest.w.Flush()
						guest.wmu.Unlock()
					}
				}()
				deadline := time.Now().Add(quick)
				var err error
				for ; time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
					if err = s.CloseStdinContext(ctx); err != nil {
						break
					}
				}
				if !errors.Is(err, c.want) {
					t.Fatalf("input after busy: %v", err)
				}
				// ...but the process runs to its exit.
				guest.event(weavewire.KindExecExit, weavewire.ExecExit{ExecID: s.ID(), Code: 3})
				if code, err := s.Wait(ctx); code != 3 || err != nil {
					t.Fatalf("Wait = %d, %v", code, err)
				}
				return
			}
			code, err := s.Wait(ctx)
			if code != -1 || !errors.Is(err, c.want) {
				t.Fatalf("Wait = %d, %v", code, err)
			}
			if _, err := io.ReadAll(s.Stdout()); !errors.Is(err, c.want) {
				t.Fatalf("stdout ended with %v", err)
			}
			if _, err := s.Write([]byte("more")); !errors.Is(err, c.want) {
				t.Fatalf("write after the session ended: %v", err)
			}
			// A late exit for it changes nothing.
			guest.event(weavewire.KindExecExit, weavewire.ExecExit{ExecID: s.ID(), Code: 0})
			if code, err := s.Wait(ctx); code != -1 || err == nil {
				t.Fatalf("Wait after a late exit = %d, %v", code, err)
			}
		})
	}
}

func TestDeliveryErrorMatching(t *testing.T) {
	e := &weaveclient.DeliveryError{Module: "weave.x", Kind: "weave.x.y", Reason: "someday"}
	if !errors.Is(e, weaveclient.ErrUndeliverable) ||
		errors.Is(e, weaveclient.ErrModuleBusy) ||
		errors.Is(e, weaveclient.ErrModuleNotInstalled) ||
		errors.Is(e, weaveclient.ErrModuleNotRunning) ||
		errors.Is(e, weaveclient.ErrNoSession) ||
		errors.Is(e, io.EOF) {
		t.Fatal("an unknown reason matched a specific sentinel")
	}
	if got := e.Error(); got != "weave: weave.x.y: core could not deliver it to weave.x: someday" {
		t.Fatalf("Error() = %q", got)
	}
	running := &weaveclient.DeliveryError{Reason: hvchannel.ReasonNotRunning, State: "backoff"}
	if errors.Is(running, weaveclient.ErrNoSession) ||
		!errors.Is(running, weaveclient.ErrModuleNotRunning) {
		t.Fatal("a module in backoff is not a missing session")
	}
}

// Modules on a closed client, and one that closes while it waits.
func TestModulesOnAClosedChannel(t *testing.T) {
	guest, client := newRaw(t)
	errs := make(chan error, 1)
	go func() { _, err := client.Modules(context.Background()); errs <- err }()
	guest.read()
	_ = guest.conn.Close()
	if err := <-errs; err == nil {
		t.Fatal("Modules survived the channel")
	}
	<-client.Done()
	if _, err := client.Modules(context.Background()); err == nil {
		t.Fatal("Modules on a closed client")
	}
}
