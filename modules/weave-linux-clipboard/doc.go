// Command weave-linux-clipboard is the clipboard capability for Linux: it
// reports, reads and replaces the console user's clipboard, holding every
// representation of a set at once as the macOS and Windows modules do. It
// speaks to the session's display server in pure Go: Wayland through the
// ext-data-control-v1 or wlr-data-control-unstable-v1 protocol, X11 (and
// XWayland, under a compositor with neither) as the CLIPBOARD selection's
// owner. On a Wayland session with neither, it falls back to wl-clipboard,
// which holds one representation per copy, and stat says so. Copied files
// stream to and from disk, staged in the console user's cache directory, or
// /var/tmp when that is held in memory (tmpfs or ramfs), so a large copy
// never fills RAM. See docs/clipboard.md in the repository.
//
// Every other file is constrained to linux. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
