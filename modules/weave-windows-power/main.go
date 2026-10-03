//go:build windows

package main

import (
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavepower"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// serve is a variable so a test can run main without Serve taking over the
// test binary's stdout and exiting it.
var serve = modulesdk.Serve

func main() { serve(newModule()) }

func newModule() *weavemodule.Module {
	return weavemodule.New(
		weavemodule.ModuleID("windows", weavewire.Power), // weave-windows-power
		newService(),
	)
}

func newService() *weavepower.Service {
	return weavepower.NewService(newPower())
}
