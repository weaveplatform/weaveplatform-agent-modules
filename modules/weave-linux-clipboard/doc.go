// Command weave-linux-clipboard is the clipboard capability for Linux: it
// reports, reads and replaces the console user's clipboard through
// wl-clipboard under Wayland or xclip under X11.
//
// Every other file is constrained to linux. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
