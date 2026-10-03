// Command weave-macos-session is the session capability for macOS: who is
// logged in at the console and in the background, as loginwindow publishes it,
// and an event whenever the console session changes. Locking answers
// unsupported: macOS has no supported way to lock a session from outside it.
//
// Every other file is constrained to darwin. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
