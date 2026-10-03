//go:build linux

package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// clock sets the Linux wall clock. Reading it is portable and weavetime does
// that itself; only the privileged step is the module's.
type clock struct {
	settime func(clockid int32, ts *unix.Timespec) error
}

func newClock() clock { return clock{settime: unix.ClockSettime} }

// SetTime steps CLOCK_REALTIME to t.
//
// A step, not a slew: a machine resuming from a snapshot can be days out, and
// adjtimex would take longer to converge than it is likely to run. It needs
// CAP_SYS_TIME, which the module's system privilege carries.
func (c clock) SetTime(_ context.Context, t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	if err := c.settime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("clock_settime: %w", err)
	}
	return nil
}
