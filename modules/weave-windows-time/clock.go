//go:build windows

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/systeminformation"
)

// systemtimePrivilege is what SetSystemTime needs, held by system and by
// administrators but disabled until enabled.
const systemtimePrivilege = "SeSystemtimePrivilege"

// clock sets the Windows wall clock through the house binding. Reading it is
// portable and weavetime does that itself; only the privileged step is the
// module's.
//
// Both steps are fields, so tests exercise SetTime without moving the clock
// of the machine running them.
type clock struct {
	enable func(name string) error
	set    func(*foundation.SYSTEMTIME) error
}

func newClock() clock {
	return clock{enable: processPrivileges().enable, set: systeminformation.SetSystemTime}
}

// SetTime steps the system clock to t.
//
// SetSystemTime takes UTC; passing local time is the classic way to move a
// machine's clock by its zone offset. A step rather than a slew through
// SetSystemTimeAdjustment: a machine resuming from a snapshot can be days out.
func (c clock) SetTime(_ context.Context, t time.Time) error {
	if err := c.enable(systemtimePrivilege); err != nil {
		return err
	}
	st := systemTime(t)
	if err := c.set(&st); err != nil {
		return fmt.Errorf("SetSystemTime: %w", err)
	}
	return nil
}

// systemTime converts t to a UTC SYSTEMTIME, to the millisecond it carries.
func systemTime(t time.Time) foundation.SYSTEMTIME {
	u := t.UTC()
	//nolint:gosec // G115: every field is a calendar component well inside uint16.
	return foundation.SYSTEMTIME{
		WYear:         uint16(u.Year()),
		WMonth:        uint16(u.Month()),
		WDayOfWeek:    uint16(u.Weekday()),
		WDay:          uint16(u.Day()),
		WHour:         uint16(u.Hour()),
		WMinute:       uint16(u.Minute()),
		WSecond:       uint16(u.Second()),
		WMilliseconds: uint16(u.Nanosecond() / int(time.Millisecond)),
	}
}
