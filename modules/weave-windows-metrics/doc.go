// Command weave-windows-metrics is the metrics capability for Windows: one live
// sample of CPU, load, memory, swap, disk and process count per request.
//
// It ships as a zip built by packaging/modulezip, whose install.ps1 installs
// it into weave-agent's modules directory, from read-only install media too.
//
// Every other file is constrained to windows. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
