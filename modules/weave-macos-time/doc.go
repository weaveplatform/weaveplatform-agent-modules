// Command weave-macos-time is the time capability for macOS: it reads the
// clock so a host can measure drift, and steps it when a suspend or snapshot
// left it behind.
//
// Every other file is constrained to darwin. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
