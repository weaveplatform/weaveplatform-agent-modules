// Command weave-windows-session is the session capability for Windows: who is
// logged in at the console and over Remote Desktop, from the Terminal Services
// API, and an event whenever the console session changes. Locking answers
// unsupported: Windows locks only the session of the process that asks.
//
// It ships as a zip built by packaging/modulezip, whose install.ps1 installs
// it into weave-agent's modules directory, from read-only install media too.
//
// Every other file is constrained to windows. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
