// Command weave-macos-display is the display capability for macOS: it lists
// the console session's displays with their modes and scale, and switches a
// display to another mode — the guest half of matching a resized VM window.
//
// It runs in the console user's session, where the window server that owns
// the display configuration is reachable.
//
// Every other file is constrained to darwin. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
