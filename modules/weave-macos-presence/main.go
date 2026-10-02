//go:build darwin

package main

import (
	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavepresence"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// serve is a variable so a test can run main without Serve taking over the
// test binary's stdout and exiting it.
var serve = modulesdk.Serve

func main() { serve(newModule()) }

func newModule() *weavemodule.Module {
	return weavemodule.New(
		weavemodule.ModuleID("macos", weavewire.Presence), // weave-macos-presence
		newService(),
	)
}

func newService() *weavepresence.Service {
	return weavepresence.NewService(newKernel(), newInventory())
}
