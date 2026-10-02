//go:build windows

package weaveexec

import "syscall"

// errTerminalHangup is what reading a closed pseudo-console pipe returns once
// the child has exited. Windows reports ERROR_BROKEN_PIPE rather than Unix's
// EIO, but it means the same thing and is equally not a failure.
var errTerminalHangup = syscall.ERROR_BROKEN_PIPE
