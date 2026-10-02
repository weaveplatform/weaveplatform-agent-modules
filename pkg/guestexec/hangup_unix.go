//go:build unix

package guestexec

import "syscall"

// errTerminalHangup is what a pseudo-terminal master read returns once the
// child has exited and closed the slave side. On Unix that is EIO, which looks
// alarming but is the ordinary end of an interactive session.
var errTerminalHangup = syscall.EIO
