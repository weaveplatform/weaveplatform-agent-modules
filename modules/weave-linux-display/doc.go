// Command weave-linux-display is the display capability for Linux: it lists
// the console session's displays with their modes and scale, and switches a
// display to another mode — the guest half of matching a resized VM window.
//
// It runs in the console user's session, with the environment core gives it
// (WAYLAND_DISPLAY or DISPLAY), and drives the session's own tool: wlr-randr
// on a wlroots compositor, xrandr on X11.
//
// Every other file is constrained to linux. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
