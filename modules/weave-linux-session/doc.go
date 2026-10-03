// Command weave-linux-session is the session capability for Linux: who is
// logged in at the console and elsewhere, according to systemd-logind, an
// event whenever the console session changes, and locking a session's
// screen.
//
// Every other file is constrained to linux. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
