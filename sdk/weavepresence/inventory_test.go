package weavepresence_test

import (
	"context"
	"os"
	"runtime"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavepresence"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

type fakeInventory struct{ called bool }

func (f *fakeInventory) Collect(_ context.Context, inv *weavewire.InventoryResponse) {
	f.called = true
	inv.SerialNumber = "SERIAL-123"
	inv.CPUModel = "Test CPU"
}

func collectOnce(t *testing.T, inv weavepresence.InventoryBackend) weavewire.InventoryResponse {
	t.Helper()
	var out weavewire.InventoryResponse
	weavemoduletest.Start(t, weavepresence.NewService(nil, inv)).
		Decode(weavewire.KindPresenceInventory, nil, &out)
	return out
}

func TestInventoryCombinesPortableAndOSHalves(t *testing.T) {
	backend := &fakeInventory{}
	inv := collectOnce(t, backend)

	if !backend.called {
		t.Fatal("the OS backend was never consulted")
	}
	if inv.SerialNumber != "SERIAL-123" || inv.CPUModel != "Test CPU" {
		t.Fatalf("OS fields missing: %+v", inv)
	}

	// The portable half must be filled without the backend doing anything.
	if inv.OS != runtime.GOOS || inv.Arch != runtime.GOARCH {
		t.Fatalf("os/arch = %s/%s, want %s/%s", inv.OS, inv.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if want, _ := os.Hostname(); inv.Hostname != want {
		t.Fatalf("hostname = %q, want %q", inv.Hostname, want)
	}
	if inv.CPUCores < 1 {
		t.Fatalf("cpu cores = %d", inv.CPUCores)
	}
	if inv.CollectedAt.IsZero() {
		t.Fatal("no collection timestamp")
	}
}

// The interface list is the reason this op exists: it is the one thing a host
// cannot learn from outside. Every machine has a loopback, so an empty list
// here means enumeration is broken, not that the guest has no network.
func TestInventoryReportsInterfaces(t *testing.T) {
	inv := collectOnce(t, &fakeInventory{})

	if len(inv.Interfaces) == 0 {
		t.Fatal("no interfaces reported — loopback alone should appear")
	}
	var sawLoopback, sawAddr bool
	for _, iface := range inv.Interfaces {
		if iface.Name == "" {
			t.Fatalf("unnamed interface in %+v", inv.Interfaces)
		}
		if iface.Loopback {
			sawLoopback = true
		}
		if len(iface.Addrs) > 0 {
			sawAddr = true
		}
	}
	if !sawLoopback {
		t.Fatal("loopback was not reported")
	}
	if !sawAddr {
		t.Fatal("no interface carried an address — the addresses are the point")
	}
}

// An OS backend that fills nothing must still yield a usable inventory: partial
// data beats a failure the host cannot act on.
func TestInventorySurvivesASilentBackend(t *testing.T) {
	inv := collectOnce(t, silentInventory{})
	if inv.Hostname == "" || inv.OS == "" {
		t.Fatalf("portable fields lost when the backend filled nothing: %+v", inv)
	}
}

type silentInventory struct{}

func (silentInventory) Collect(context.Context, *weavewire.InventoryResponse) {}
