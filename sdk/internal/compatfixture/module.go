package main

import (
	"context"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
)

// ModuleID is the fixture's manifest id. Core launches a module only under
// the id its manifest declares, and the runtime refuses an Init for any
// other, so the test's manifest and the binary must agree on it.
const ModuleID = "weave-compat-fixture"

// fixture reports healthy once core's module registry lists it. Reaching
// that state under a released core proves the handshake, Init, Start, the
// host connection, the registry host service and the health poll all agree
// with this sdk.
type fixture struct {
	host modulesdk.Host

	mu sync.Mutex
	// seen is set once the registry has listed this module; why is what
	// the last look at the registry said otherwise, for the health reason.
	seen   bool
	why    string
	cancel context.CancelFunc
	done   chan struct{}
}

func (f *fixture) ID() string { return ModuleID }

// Requires nothing: a capability core could not probe on a CI runner would
// park the module in requirements-unmet, which is not what this measures.
func (f *fixture) Requires() []modulesdk.Capability { return nil }

func (f *fixture) Init(_ context.Context, host modulesdk.Host) error {
	f.host = host
	f.why = "registry: not looked yet"
	host.Log().Info("compat fixture initialised", "protocol", modulesdk.Protocol)
	return nil
}

// Start watches core's registry for this module in the background: core
// lists a module from launch, but Start is no place to wait on core.
func (f *fixture) Start(ctx context.Context) error {
	// Start's context ends when Start returns; the watch outlives it.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f.mu.Lock()
	f.cancel = cancel
	f.done = make(chan struct{})
	f.mu.Unlock()
	go f.watch(ctx)
	return nil
}

func (f *fixture) watch(ctx context.Context) {
	defer close(f.done)
	snaps, err := f.host.Registry().Watch(ctx)
	if err != nil {
		f.note(false, "registry: "+err.Error())
		return
	}
	for snap := range snaps {
		m, ok := snap.Module(ModuleID)
		if ok {
			f.host.Log().Info("the registry lists the fixture",
				"revision", snap.Revision, "state", m.State, "address", m.Address)
			f.note(true, "")
			return
		}
		f.note(false, "registry: not listed")
	}
}

func (f *fixture) note(seen bool, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen, f.why = seen, why
}

func (f *fixture) Stop(context.Context) error {
	f.mu.Lock()
	cancel, done := f.cancel, f.done
	f.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return nil
}

func (f *fixture) Health() modulesdk.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.seen {
		return modulesdk.Health{Status: modulesdk.HealthDegraded, Reason: f.why}
	}
	return modulesdk.Health{Status: modulesdk.HealthHealthy}
}
