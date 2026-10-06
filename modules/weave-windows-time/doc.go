// Command weave-windows-time is the time capability for Windows: it reads the
// clock so a host can measure drift, and steps it when a suspend or snapshot
// left it behind.
//
// It ships as a zip built by packaging/modulezip, whose install.ps1 installs
// it into weave-agent's modules directory, from read-only install media too.
//
// Every other file is constrained to windows. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
