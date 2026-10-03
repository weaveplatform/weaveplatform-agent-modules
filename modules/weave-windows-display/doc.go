// Command weave-windows-display is the display capability for Windows: it lists
// the console session's displays with their modes and scale, and switches a
// display to another mode — the guest half of matching a resized VM window.
//
// It runs in the console user's session, on the interactive desktop:
// display settings are applied per user, and session 0 has no displays.
//
// Every other file is constrained to windows. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
