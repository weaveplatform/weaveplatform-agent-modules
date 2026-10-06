// Command weave-windows-clipboard is the clipboard capability for Windows: it
// reports, reads and replaces the console user's clipboard through the Win32
// clipboard API.
//
// Core launches it as the console user, which is neither SYSTEM nor an
// elevated Administrator, so the module's own pipe must admit that user as
// well as core: the SDK's ipc.Listen grants the process's own user SID.
//
// Every other file is constrained to windows. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
