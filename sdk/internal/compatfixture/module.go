package main

import (
	"context"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
)

// ModuleID is the fixture's manifest id. Core launches a module only under
// the id its manifest declares, and the runtime refuses an Init for any
// other, so the test's manifest and the binary must agree on it.
const ModuleID = "weave-compat-fixture"

// fixture reports healthy from the moment core starts it. Reaching that
// state under a released core proves the handshake, Init, Start, the host
// connection and the health poll all agree with this sdk.
type fixture struct {
	host modulesdk.Host
}

func (f *fixture) ID() string { return ModuleID }

// Requires nothing: a capability core could not probe on a CI runner would
// park the module in requirements-unmet, which is not what this measures.
func (f *fixture) Requires() []modulesdk.Capability { return nil }

func (f *fixture) Init(_ context.Context, host modulesdk.Host) error {
	f.host = host
	host.Log().Info("compat fixture initialised", "protocol", modulesdk.Protocol)
	return nil
}

func (f *fixture) Start(context.Context) error { return nil }

func (f *fixture) Stop(context.Context) error { return nil }

func (f *fixture) Health() modulesdk.Health {
	return modulesdk.Health{Status: modulesdk.HealthHealthy}
}
