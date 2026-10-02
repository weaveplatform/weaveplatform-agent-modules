package weaveclient_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavepower"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavepresence"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// This file drives the two ends against each other over a real pipe with real
// framing: weaveclient.Client → hvchannel → a stand-in for core that routes by
// capability address → one module per capability, and back. It is the test
// that catches a drift between the ends, which nothing on the channel itself
// would detect — there is no negotiation, so a mismatch just looks like a
// guest that never answers.

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

type fakeKernel struct{}

func (fakeKernel) Kernel(context.Context) (string, error) { return "test-kernel", nil }

// wire stands up a guest serving presence plus svcs, and returns the host
// client connected to it.
func wire(t *testing.T, svcs ...weavemodule.Service) *weaveclient.Client {
	t.Helper()
	return wireWith(
		t,
		nil,
		append([]weavemodule.Service{weavepresence.NewService(fakeKernel{}, nil)}, svcs...)...)
}

func wireWith(
	t *testing.T,
	trusted ed25519.PublicKey,
	svcs ...weavemodule.Service,
) *weaveclient.Client {
	t.Helper()
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, trusted)
	for _, svc := range svcs {
		core.Serve(t, svc)
	}
	go core.Run()

	ctx, cancel := context.WithCancel(context.Background())
	client := weaveclient.New(ctx, hostConn, weaveclient.Options{Log: quietLog()})
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		_ = core.Close()
	})
	return client
}

func TestHelloAcrossTheRealFraming(t *testing.T) {
	client := wire(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hello, err := client.Hello(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Version != weavewire.ProtocolVersion {
		t.Fatalf("version = %q, want %q", hello.Version, weavewire.ProtocolVersion)
	}
	if hello.OS != runtime.GOOS || hello.Arch != runtime.GOARCH {
		t.Fatalf("os/arch = %s/%s, want %s/%s", hello.OS, hello.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if hello.Kernel != "test-kernel" {
		t.Fatalf("kernel = %q — the backend was not consulted", hello.Kernel)
	}
}

type stubInventory struct{}

func (stubInventory) Collect(_ context.Context, inv *weavewire.InventoryResponse) {
	inv.SerialNumber = "LOOPBACK-1"
	inv.CPUModel = "Stub CPU"
}

type stubPower struct{ ran chan string }

func (p stubPower) act(op string) (weavepower.Action, error) {
	return weavepower.Action{
		Command: "stub " + op,
		Run:     func() error { p.ran <- op; return nil },
	}, nil
}

func (p stubPower) Shutdown(context.Context, string) (weavepower.Action, error) {
	return p.act("shutdown")
}

func (p stubPower) Restart(context.Context, string) (weavepower.Action, error) {
	return p.act("restart")
}

// Inventory across the real framing. The interface list is the payload that
// matters: it is what `weave ip` needs and the one thing the host cannot learn
// from outside the guest.
func TestInventoryAcrossTheRealFraming(t *testing.T) {
	client := wireWith(t, nil, weavepresence.NewService(fakeKernel{}, stubInventory{}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	inv, err := client.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inv.SerialNumber != "LOOPBACK-1" || inv.CPUModel != "Stub CPU" {
		t.Fatalf("OS half missing: %+v", inv)
	}
	if inv.OS != runtime.GOOS || inv.Hostname == "" {
		t.Fatalf("portable half missing: %+v", inv)
	}
	if len(inv.Interfaces) == 0 {
		t.Fatal("no interfaces survived the round trip")
	}
	// Interfaces carry nested slices; a marshalling mistake shows up as an
	// interface with a name and nothing else.
	var sawAddr bool
	for _, iface := range inv.Interfaces {
		if len(iface.Addrs) > 0 {
			sawAddr = true
		}
	}
	if !sawAddr {
		t.Fatal("interfaces round-tripped without any addresses")
	}
}

// Power end to end: the host must receive its acknowledgement, and the guest
// must act only afterwards.
func TestPowerAcrossTheRealFraming(t *testing.T) {
	ran := make(chan string, 1)
	client := wire(t, weavepower.NewService(stubPower{ran: ran}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	accepted, err := client.Shutdown(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("the guest did not accept the shutdown")
	}

	// The reply arrived first — we are holding it. The action follows.
	select {
	case op := <-ran:
		if op != "shutdown" {
			t.Fatalf("guest ran %q", op)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the guest acknowledged but never acted")
	}
}

// Concurrent calls must be correlated, not serialised. This is the property
// that lets a shutdown be issued while a long exec is still streaming; if ids
// were ignored, the replies would be matched to the wrong callers.
func TestConcurrentCallsAreCorrelated(t *testing.T) {
	client := wire(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const n = 16
	errs := make(chan error, n)
	for range n {
		go func() {
			hello, err := client.Hello(ctx)
			if err != nil {
				errs <- err
				return
			}
			if hello.Kernel != "test-kernel" {
				errs <- errors.New("wrong payload returned to a concurrent caller")
				return
			}
			errs <- nil
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// An op the guest has no handler for must come back as a guest-side refusal,
// distinguishable from the channel breaking. A host that cannot tell those
// apart will hard-stop a VM that merely declined a request.
func TestUnknownOpIsAGuestErrorNotAChannelError(t *testing.T) {
	client := wire(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Call(ctx, "weave.presence.nosuch", nil, nil)
	if err == nil {
		t.Fatal("an unregistered kind was answered")
	}
	var guestErr *weaveclient.GuestError
	if !errors.As(err, &guestErr) {
		t.Fatalf("err = %T (%v), want *weaveclient.GuestError", err, err)
	}
	if client.Err() != nil {
		t.Fatalf("the channel was torn down by a refused op: %v", client.Err())
	}
}

// When the channel dies, every waiting call must fail promptly rather than
// hanging until its own context expires — a caller blocked on a dead VM is the
// bug this prevents.
func TestPendingCallsFailWhenTheChannelDies(t *testing.T) {
	guestConn, hostConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := weaveclient.New(ctx, hostConn, weaveclient.Options{Log: quietLog()})

	// Nothing answers on the guest end; kill it while a call is in flight.
	done := make(chan error, 1)
	go func() {
		_, err := client.Hello(context.Background())
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = guestConn.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call on a dead channel reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a pending call did not fail when the channel died")
	}

	// And a later call fails fast rather than waiting for its own timeout.
	if _, err := client.Hello(context.Background()); err == nil {
		t.Fatal("a call after the channel died reported success")
	}
}

func TestCallRejectsAReplyKind(t *testing.T) {
	client := wire(t)
	err := client.Call(
		context.Background(),
		weavewire.ResultKind(weavewire.KindPresenceHello),
		nil,
		nil,
	)
	if err == nil {
		t.Fatal("a reply kind was accepted as a command")
	}
}
