package modulesdk

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/werror"
)

func TestRegistryClientList(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	ctx := ctxT(t)

	snap, err := hc.Registry().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != 3 || len(snap.Modules) != 2 {
		t.Fatalf("List = %+v", snap)
	}
	clip, ok := snap.Module("weave.clipboard")
	if !ok || clip.ID != "weave-linux-clipboard" || clip.Version != "0.4.0" ||
		clip.State != ModuleStateWaitingForSession || clip.Running() ||
		clip.Health.Status != HealthUnknown {
		t.Fatalf("clipboard = %+v, %v", clip, ok)
	}
	power, ok := snap.Module("weave-linux-power") // by id, too
	if !ok || !power.Running() || power.Health.Status != HealthDegraded ||
		power.Health.Reason != "slow" || power.Health.Details["a"] != "b" {
		t.Fatalf("power = %+v, %v", power, ok)
	}
	if !snap.Installed("weave.power") || snap.Installed("weave.exec") {
		t.Fatal("Installed disagrees with the snapshot")
	}
	h.locked(func() {
		if h.badToken != 0 {
			t.Errorf("%d calls arrived without the handshake token", h.badToken)
		}
	})

	h.setFail(codes.Unavailable)
	if _, err := hc.Registry().List(ctx); !errors.Is(err, werror.ErrUnavailable) ||
		errors.Is(err, ErrRegistryUnsupported) {
		t.Fatalf("List err = %v, want ErrUnavailable", err)
	}
}

func TestRegistryClientWatch(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)

	ctx, cancel := context.WithCancel(ctxT(t))
	ch, err := hc.Registry().Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []uint64{1, 2} {
		if s := <-ch; s.Revision != want || len(s.Modules) != 2 {
			t.Fatalf("watched %+v, want revision %d", s, want)
		}
	}
	cancel()
	for range ch {
	}

	// Cancelling while a snapshot waits to be read still closes the channel.
	ctx2, cancel2 := context.WithCancel(ctxT(t))
	ch2, err := hc.Registry().Watch(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel2()
	for range ch2 {
	}
}

// The watch channel closes when the host goes away mid-stream.
func TestRegistryWatchClosesWhenHostGoes(t *testing.T) {
	h := newFakeHost(t)
	hc := h.dial(t)
	ch, err := hc.Registry().Watch(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	<-ch
	h.srv.Stop()
	for range ch {
	}
}

// A core from before weave-agent v0.9.2 has no RegistryService: both calls
// say so with ErrRegistryUnsupported, Watch before it returns a channel.
func TestRegistryUnsupported(t *testing.T) {
	h := newFakeHost(t)
	h.locked(func() { h.noRegistry = true })
	hc := h.dial(t)
	ctx := ctxT(t)
	if _, err := hc.Registry().List(ctx); !errors.Is(err, ErrRegistryUnsupported) {
		t.Fatalf("List err = %v", err)
	}
	if ch, err := hc.Registry().Watch(ctx); !errors.Is(err, ErrRegistryUnsupported) || ch != nil {
		t.Fatalf("Watch = %v, %v", ch, err)
	}

	hc.Close()
	if _, err := hc.Registry().Watch(ctx); err == nil {
		t.Fatal("Watch on a closed connection")
	}
}

func TestHealthStatusFromTheWire(t *testing.T) {
	for wire, want := range map[agentv1.Health_Status]HealthStatus{
		agentv1.Health_STATUS_UNSPECIFIED: HealthUnknown,
		agentv1.Health_STATUS_HEALTHY:     HealthHealthy,
		agentv1.Health_STATUS_DEGRADED:    HealthDegraded,
		agentv1.Health_STATUS_UNHEALTHY:   HealthUnhealthy,
		agentv1.Health_Status(99):         HealthUnknown,
	} {
		if got := healthStatus(wire); got != want {
			t.Errorf("healthStatus(%v) = %v, want %v", wire, got, want)
		}
	}
	var empty RegistrySnapshot
	if _, ok := empty.Module("x"); ok {
		t.Fatal("an empty snapshot found a module")
	}
}
