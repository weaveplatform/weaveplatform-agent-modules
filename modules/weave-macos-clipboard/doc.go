// Command weave-macos-clipboard is the clipboard capability for macOS: it
// reports, reads and replaces the console user's pasteboard through
// NSPasteboard, the general pasteboard of AppKit.
//
// Every other file is constrained to darwin. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
