// Command weave-windows-exec is the exec capability for Windows: it runs the
// program a host names, with pipes or a pseudo-console (ConPTY), and streams its
// stdio and exit status back over the channel.
//
// It ships as a zip built by packaging/modulezip, whose install.ps1 installs
// it into weave-agent's modules directory, from read-only install media too.
//
// Every other file is constrained to windows. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
