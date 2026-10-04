package weavemoduletest_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// fakeTB records fatal failures and ends the calling goroutine the way
// testing.T does, so the harness's own failure paths can be exercised.
type fakeTB struct {
	testing.TB
	mu       sync.Mutex
	failures []string
	cleanups []func()
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.mu.Lock()
	f.failures = append(f.failures, fmt.Sprintf(format, args...))
	f.mu.Unlock()
	runtime.Goexit()
}

func (f *fakeTB) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }

// run executes fn as a test body on its own goroutine and reports whether it
// failed.
func (f *fakeTB) run(fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	<-done
	for _, c := range f.cleanups {
		c()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.failures) > 0
}

type timeService struct {
	register func(r *weavemodule.Registrar) error
}

func (timeService) Capability() weavewire.Capability { return weavewire.Time }
func (s timeService) Register(r *weavemodule.Registrar) error {
	if s.register != nil {
		return s.register(r)
	}
	r.Handle(
		weavewire.KindTimeGet,
		func(context.Context, []byte) ([]byte, error) { return []byte(`{"unix_nano":1}`), nil },
	)
	r.Handle(
		weavewire.KindTimeSet,
		func(context.Context, []byte) ([]byte, error) { return nil, errors.New("read-only") },
	)
	return nil
}

func TestHarnessFailurePaths(t *testing.T) {
	bad := timeService{register: func(*weavemodule.Registrar) error { return errors.New("no") }}
	for name, body := range map[string]func(tb *fakeTB){
		"init fails": func(tb *fakeTB) { weavemoduletest.Start(tb, bad) },
		"guest error": func(tb *fakeTB) {
			weavemoduletest.Start(tb, timeService{}).Decode(weavewire.KindTimeSet, nil, &struct{}{})
		},
		"undecodable": func(tb *fakeTB) {
			weavemoduletest.Start(tb, timeService{}).Decode(weavewire.KindTimeGet, nil, &[]int{})
		},
		"bad payload":     func(tb *fakeTB) { weavemoduletest.Start(tb, timeService{}).Notify(weavewire.KindTimeGet, func() {}) },
		"no result":       func(tb *fakeTB) { slowCall(tb) },
		"core init fails": func(tb *fakeTB) { weavemoduletest.NewCore(nil, nil).Serve(tb, bad) },
	} {
		tb := &fakeTB{TB: t}
		if !tb.run(func() { body(tb) }) {
			t.Errorf("%s: did not fail the test", name)
		}
	}
}

// slowCall waits on a reply that never comes: the transport drops it.
func slowCall(tb *fakeTB) {
	svc := timeService{register: func(r *weavemodule.Registrar) error {
		r.Handle(weavewire.KindTimeGet, func(ctx context.Context, _ []byte) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		r.Handle(
			weavewire.KindTimeSet,
			func(context.Context, []byte) ([]byte, error) { return nil, nil },
		)
		return nil
	}}
	h := weavemoduletest.Start(tb, svc, func(host *weavemoduletest.Host) {
		host.T.OnSend = func(modulesdk.Message) error { return errors.New("dropped") }
	})
	h.Timeout = 50 * time.Millisecond
	h.Call(weavewire.KindTimeSet, nil)
}

func TestHostSurfaces(t *testing.T) {
	host := weavemoduletest.NewHost(weavemoduletest.NewTransport())
	if host.Identity() != nil || host.Store("") != nil || host.UI() != nil || host.Policy() != nil {
		t.Fatal("a guest host exposes a surface guest modules do not have")
	}
	host.SetPolicy([]byte(`{}`), nil)
	if _, err := host.Policy().Watch(context.Background()); err == nil {
		t.Fatal("watch is not modelled")
	}
	if _, err := host.Events().Subscribe(context.Background(), "x"); err == nil {
		t.Fatal("subscribe is not modelled")
	}
	if err := host.Events().
		Publish(context.Background(), "t", []byte("d")); err != nil ||
		len(host.Published()) != 1 {
		t.Fatalf("publish = %v, %v", err, host.Published())
	}
	tr := weavemoduletest.NewTransport()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err := tr.Send(ctx, modulesdk.Message{}, false); ok || err == nil {
		t.Fatal("a cancelled send was delivered")
	}
	if tr.WaitFor(10*time.Millisecond, func(s []modulesdk.Message) bool { return len(s) > 0 }) {
		t.Fatal("WaitFor saw a message that was never sent")
	}
}

// Core routes by address and gates on authentication exactly as agent-core.
func TestCoreRoutesByAddressAndGates(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	core.Serve(t, timeService{})
	go core.Run()
	defer core.Close()

	r, w := bufio.NewReader(hostConn), bufio.NewWriter(hostConn)
	send := func(module, kind string, data []byte) {
		t.Helper()
		if err := hvchannel.WriteEnvelope(
			w,
			hvchannel.Envelope{Module: module, Kind: kind, Data: data},
		); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() hvchannel.Envelope {
		t.Helper()
		env, err := hvchannel.ReadEnvelope(r)
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
	cmd, _ := weavewire.EncodeCommand("1", nil)

	send("weave.time", weavewire.KindTimeGet, cmd)
	if env := recv(); env.Kind != hvchannel.KindAuthResult {
		t.Fatalf("unauthenticated op answered with %s", env.Kind)
	}

	// Garbage and a malformed response cost one frame each, not the channel.
	if err := hvchannel.WriteFrame(w, []byte("{")); err != nil {
		t.Fatal(err)
	}
	send(hvchannel.ControlModule, hvchannel.KindAuthResponse, []byte("["))
	if env := recv(); env.Kind != hvchannel.KindAuthResult {
		t.Fatalf("malformed response answered with %s", env.Kind)
	}
	send(hvchannel.ControlModule, "auth.unknown", nil)

	send(hvchannel.ControlModule, hvchannel.KindAuthBegin, nil)
	var challenge hvchannel.AuthChallenge
	_ = json.Unmarshal(recv().Data, &challenge)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	resp, _ := hvchannel.Sign(wrong, challenge)
	data, _ := json.Marshal(resp)
	send(hvchannel.ControlModule, hvchannel.KindAuthResponse, data)
	var result hvchannel.AuthResult
	if _ = json.Unmarshal(recv().Data, &result); result.OK {
		t.Fatal("the wrong key authenticated")
	}

	send(hvchannel.ControlModule, hvchannel.KindAuthBegin, nil)
	_ = json.Unmarshal(recv().Data, &challenge)
	resp, _ = hvchannel.Sign(priv, challenge)
	data, _ = json.Marshal(resp)
	send(hvchannel.ControlModule, hvchannel.KindAuthResponse, data)
	if _ = json.Unmarshal(recv().Data, &result); !result.OK {
		t.Fatalf("the right key was refused: %s", result.Reason)
	}

	// No module answers to this address: core says so, now that the host
	// has authenticated.
	send("weave.exec", weavewire.KindExecStart, cmd)
	if env := recv(); env.Module != hvchannel.ControlModule ||
		env.Kind != hvchannel.KindDeliveryFailed {
		t.Fatalf("an undeliverable frame answered with %s/%s", env.Module, env.Kind)
	}
	send("weave.time", weavewire.KindTimeGet, cmd)
	env := recv()
	if env.Module != "weave.time" || env.Kind != weavewire.ResultKind(weavewire.KindTimeGet) {
		t.Fatalf("got %s/%s", env.Module, env.Kind)
	}
}

// rawHost is the host end of a channel to a Core, frame by frame.
type rawHost struct {
	t *testing.T
	r *bufio.Reader
	w *bufio.Writer
}

func newRawHost(
	t *testing.T,
	trusted ed25519.PublicKey,
	legacy bool,
) (*rawHost, *weavemoduletest.Core) {
	t.Helper()
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, trusted)
	core.Legacy = legacy
	core.Serve(t, timeService{})
	go core.Run()
	t.Cleanup(func() { _ = core.Close(); _ = hostConn.Close() })
	return &rawHost{t: t, r: bufio.NewReader(hostConn), w: bufio.NewWriter(hostConn)}, core
}

func (h *rawHost) send(env hvchannel.Envelope) {
	h.t.Helper()
	if err := hvchannel.WriteEnvelope(h.w, env); err != nil {
		h.t.Fatal(err)
	}
	if err := h.w.Flush(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *rawHost) recv() hvchannel.Envelope {
	h.t.Helper()
	env, err := hvchannel.ReadEnvelope(h.r)
	if err != nil {
		h.t.Fatal(err)
	}
	return env
}

// authenticate runs the handshake with priv.
func (h *rawHost) authenticate(priv ed25519.PrivateKey) {
	h.t.Helper()
	h.send(
		hvchannel.Envelope{Module: hvchannel.ControlModule, Kind: hvchannel.KindAuthBegin, ID: "b"},
	)
	var challenge hvchannel.AuthChallenge
	if env := h.recv(); env.ID != "b" || json.Unmarshal(env.Data, &challenge) != nil {
		h.t.Fatalf("challenge %+v", env)
	}
	resp, _ := hvchannel.Sign(priv, challenge)
	data, _ := json.Marshal(resp)
	h.send(
		hvchannel.Envelope{
			Module: hvchannel.ControlModule,
			Kind:   hvchannel.KindAuthResponse,
			Data:   data,
			ID:     "r",
		},
	)
	var result hvchannel.AuthResult
	if env := h.recv(); env.ID != "r" || json.Unmarshal(env.Data, &result) != nil || !result.OK {
		h.t.Fatalf("auth result %+v", env)
	}
}

func decodeSnapshot(t *testing.T, env hvchannel.Envelope) hvchannel.ModulesSnapshot {
	t.Helper()
	var snap hvchannel.ModulesSnapshot
	if err := json.Unmarshal(env.Data, &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

// The registry as weave-agent v0.9.2 serves it: refused before
// authentication with the id echoed, then listed, pushed on change, and
// consulted to say why a frame cannot be delivered.
func TestCoreModuleRegistry(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h, core := newRawHost(t, pub, false)

	list := hvchannel.Envelope{
		Module: hvchannel.ControlModule,
		Kind:   hvchannel.KindModulesList,
		ID:     "l1",
	}
	h.send(list)
	var refusal hvchannel.AuthResult
	if env := h.recv(); env.Kind != hvchannel.KindAuthResult || env.ID != "l1" ||
		json.Unmarshal(env.Data, &refusal) != nil || refusal.OK {
		t.Fatalf("pre-auth modules.list answered %+v", env)
	}
	// A hello for a missing module before authentication goes unanswered;
	// the time result after it is the next frame.
	h.send(hvchannel.Envelope{Module: "weave.presence", Kind: "weave.presence.hello", ID: "h"})

	h.authenticate(priv)

	list.ID = "l2"
	h.send(list)
	env := h.recv()
	snap := decodeSnapshot(t, env)
	if env.Kind != hvchannel.KindModulesListResult || env.ID != "l2" || snap.Revision == 0 ||
		len(snap.Modules) != 1 || snap.Modules[0].Address != "weave.time" ||
		snap.Modules[0].State != "running" || snap.Modules[0].Health.Status != hvchannel.HealthHealthy {
		t.Fatalf("modules.list answered %s %+v", env.Kind, snap)
	}

	// A change is pushed, without an id, sorted by module id.
	go core.SetModule(hvchannel.ModuleInfo{
		ID: "a-clipboard", Address: "weave.clipboard", State: "waiting-for-session",
		Detail: "no console user session",
	})
	env = h.recv()
	pushed := decodeSnapshot(t, env)
	if env.Kind != hvchannel.KindModulesChanged || env.ID != "" ||
		pushed.Revision <= snap.Revision || len(pushed.Modules) != 2 ||
		pushed.Modules[0].ID != "a-clipboard" || pushed.Modules[0].Since == "" ||
		pushed.Modules[0].Health.Status != hvchannel.HealthUnknown ||
		pushed.Modules[0].Capabilities == nil {
		t.Fatalf("pushed %s %+v", env.Kind, pushed)
	}

	cmd, _ := weavewire.EncodeCommand("c", nil)
	cases := []struct {
		module, reason, state string
		setup                 func()
	}{
		{module: "weave.exec", reason: hvchannel.ReasonNotInstalled},
		{
			module: "weave.clipboard",
			reason: hvchannel.ReasonNotRunning,
			state:  "waiting-for-session",
		},
		{
			module: "weave.time",
			reason: hvchannel.ReasonBusy,
			setup:  func() { core.SetBusy("weave.time", true) },
		},
	}
	for _, c := range cases {
		if c.setup != nil {
			c.setup()
		}
		h.send(
			hvchannel.Envelope{
				Module: c.module,
				Kind:   c.module + ".op",
				Data:   cmd,
				ID:     "d-" + c.module,
			},
		)
		env := h.recv()
		var df hvchannel.DeliveryFailed
		if env.Kind != hvchannel.KindDeliveryFailed || env.ID != "d-"+c.module ||
			json.Unmarshal(env.Data, &df) != nil || df.Reason != c.reason || df.State != c.state ||
			df.Module != c.module || df.Kind != c.module+".op" {
			t.Fatalf("%s: %s %s %+v", c.module, env.Kind, env.ID, df)
		}
	}
	core.SetBusy("weave.time", false)

	go core.RemoveModule("a-clipboard")
	if snap := decodeSnapshot(
		t,
		h.recv(),
	); len(snap.Modules) != 1 ||
		snap.Revision <= pushed.Revision {
		t.Fatalf("after remove %+v", snap)
	}
	if got := core.Modules(); len(got.Modules) != 1 {
		t.Fatalf("Modules() = %+v", got)
	}
	go core.PushModules()
	if env := h.recv(); env.Kind != hvchannel.KindModulesChanged {
		t.Fatalf("PushModules sent %s", env.Kind)
	}
}

// A Legacy core is weave-agent before v0.9.2: modules.list is ignored, ids
// are not echoed, and an undeliverable frame is dropped in silence.
func TestCoreLegacy(t *testing.T) {
	h, core := newRawHost(t, nil, true)
	cmd, _ := weavewire.EncodeCommand("c", nil)
	h.send(
		hvchannel.Envelope{
			Module: hvchannel.ControlModule,
			Kind:   hvchannel.KindModulesList,
			ID:     "l",
		},
	)
	h.send(
		hvchannel.Envelope{Module: "weave.exec", Kind: weavewire.KindExecStart, Data: cmd, ID: "x"},
	)
	core.SetModule(hvchannel.ModuleInfo{ID: "extra", Address: "weave.extra"})
	h.send(
		hvchannel.Envelope{Module: hvchannel.ControlModule, Kind: hvchannel.KindAuthBegin, ID: "b"},
	)
	if env := h.recv(); env.Kind != hvchannel.KindAuthChallenge || env.ID != "" {
		t.Fatalf("legacy core answered %s id %q", env.Kind, env.ID)
	}
}

// A module whose sends fail (the channel is gone) reports it rather than
// claiming delivery.
func TestCoreSendFailsOnAClosedChannel(t *testing.T) {
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, nil)
	var emit func(ctx context.Context) error
	core.Serve(t, timeService{register: func(r *weavemodule.Registrar) error {
		e := r.Emitter()
		emit = func(ctx context.Context) error { return e.Emit(ctx, "weave.time.drift", nil) }
		r.Handle(weavewire.KindTimeGet, nil)
		r.Handle(weavewire.KindTimeSet, nil)
		return nil
	}})
	_ = hostConn.Close()
	_ = core.Close()
	if err := emit(context.Background()); err == nil {
		t.Fatal("a send on a closed channel succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := emit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled send = %v", err)
	}
}
