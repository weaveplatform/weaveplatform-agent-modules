package weavemoduletest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
)

// A module test sets what is installed beside the module under test, and the
// module reads it through its Host as it would from core.
func TestHostRegistry(t *testing.T) {
	h := weavemoduletest.NewHost(weavemoduletest.NewTransport())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	snap, err := h.Registry().List(ctx)
	if err != nil || snap.Revision != 0 || len(snap.Modules) != 0 {
		t.Fatalf("empty registry = %+v, %v", snap, err)
	}

	h.Modules.Set(
		modulesdk.RegisteredModule{
			ID:      "z",
			Address: "weave.z",
			State:   modulesdk.ModuleStateRunning,
		},
		modulesdk.RegisteredModule{
			ID:      "a",
			Address: "weave.a",
			State:   modulesdk.ModuleStateStopped,
		},
	)
	snap, _ = h.Registry().List(ctx)
	if snap.Revision != 1 || len(snap.Modules) != 2 || snap.Modules[0].ID != "a" {
		t.Fatalf("after Set = %+v", snap)
	}
	if m, ok := snap.Module("weave.z"); !ok || !m.Running() {
		t.Fatalf("weave.z = %+v, %v", m, ok)
	}

	wctx, wcancel := context.WithCancel(ctx)
	ch, err := h.Registry().Watch(wctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := <-ch; s.Revision != 1 {
		t.Fatalf("first watched = %+v", s)
	}
	h.Modules.SetModule(modulesdk.RegisteredModule{ID: "b", Address: "weave.b"})
	if s := <-ch; s.Revision != 2 || !s.Installed("weave.b") {
		t.Fatalf("after SetModule = %+v", s)
	}
	h.Modules.Remove("z")
	if s := <-ch; s.Revision != 3 || s.Installed("weave.z") {
		t.Fatalf("after Remove = %+v", s)
	}
	wcancel()
	for range ch {
	}

	// Cancelled while a snapshot waits to be read.
	wctx2, wcancel2 := context.WithCancel(ctx)
	ch2, _ := h.Registry().Watch(wctx2)
	wcancel2()
	for range ch2 {
	}

	unsupported := modulesdk.ErrRegistryUnsupported
	h.Modules.Fail(unsupported)
	if _, err := h.Registry().List(ctx); !errors.Is(err, unsupported) {
		t.Fatalf("List = %v", err)
	}
	if _, err := h.Registry().Watch(ctx); !errors.Is(err, unsupported) {
		t.Fatalf("Watch = %v", err)
	}
	h.Modules.Fail(nil)
	if _, err := h.Registry().List(ctx); err != nil {
		t.Fatal(err)
	}
}
