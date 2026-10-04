// Command compatfixture is the module the agent compatibility workflow runs
// under a released weave-agent: the smallest module this sdk can build that
// core will launch, initialise, start and report healthy. It needs no
// capability and touches no host service beyond logging and the module
// registry — where it must find itself before it reports healthy — so a
// failure points at the protocol between this sdk and core rather than at the
// module.
//
// TestUnderReleasedAgent drives it; see that test for how core is set up.
package main

import "github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"

func main() { modulesdk.Serve(&fixture{}) }
