// Command weave-macos-presence is the presence capability for macOS: hello,
// which a host may send before the channel authenticates, and inventory, the
// machine's own account of what it is and which addresses it holds.
//
// Every other file is constrained to darwin. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary that answers presence with another OS's facts.
package main
