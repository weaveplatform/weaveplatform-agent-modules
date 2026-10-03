// Package modulesdk is the module runtime. A module author implements the
// Module interface and calls Serve:
//
//	func main() { modulesdk.Serve(example.New()) }
//
// Serve reads the handshake environment, negotiates the protocol, listens
// on the module's socket, prints the one-line handshake answer, and
// dispatches core's lifecycle RPCs onto the Module interface. The Host
// handed to Init is a client of core's per-module host services.
package modulesdk
