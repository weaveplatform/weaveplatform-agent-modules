// Command weave-macos-exec is the exec capability for macOS: it runs the
// program a host names, with pipes or a pseudo-terminal, and streams its
// stdio and exit status back over the channel.
//
// Every other file is constrained to darwin. The module is built, tested and
// released only for the OS its manifest names, so building it for another OS
// fails at link time ("function main is undeclared") rather than producing a
// binary for an OS it was never tested on.
package main
