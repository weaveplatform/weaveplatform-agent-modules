// Command weave-windows-presence is the presence capability for Windows: hello,
// which a host may send before the channel authenticates, and inventory, the
// machine's own account of what it is and which addresses it holds.
//
// It ships as a zip built by packaging/modulezip, whose install.ps1 installs
// it into weave-agent's modules directory, from read-only install media too.
//
// Every other file is constrained to windows. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary that answers presence with another OS's facts.
package main
