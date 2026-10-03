//go:build darwin

package main

import (
	"context"
	"fmt"
	"syscall"
	"time"
)

// clock sets the macOS wall clock. Reading it is portable and weavetime does
// that itself; only the privileged step is the module's.
//
// settimeofday(2) has no wrapper in go-bindings-macosplatform (its bsd
// library is empty, and neither Foundation nor Mach sets the calendar clock),
// so it comes from the standard library's syscall, as presence's sysctls do;
// the module needs no x/sys.
type clock struct {
	settimeofday func(tv *syscall.Timeval) error
}

func newClock() clock { return clock{settimeofday: syscall.Settimeofday} }

// SetTime steps the clock to t.
//
// A step, not a slew: a machine resuming from a snapshot can be days out, and
// adjtime(2) would take longer to converge than it is likely to run. It needs
// root, which the module's system privilege is.
func (c clock) SetTime(_ context.Context, t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	if err := c.settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday: %w", err)
	}
	return nil
}
