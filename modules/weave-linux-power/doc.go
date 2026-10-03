// Command weave-linux-power is the power capability for Linux: an orderly
// shutdown or restart, acknowledged to the host before the OS goes down.
//
// Every other file is constrained to linux. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
