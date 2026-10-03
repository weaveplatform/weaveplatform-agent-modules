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

	// No module answers to this address: dropped, as core drops it.
	send("weave.exec", weavewire.KindExecStart, cmd)
	send("weave.time", weavewire.KindTimeGet, cmd)
	env := recv()
	if env.Module != "weave.time" || env.Kind != weavewire.ResultKind(weavewire.KindTimeGet) {
		t.Fatalf("got %s/%s", env.Module, env.Kind)
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
