//go:build windows

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
)

const wantPrivilege = systemtimePrivilege

// The clock is never actually set by a test: a stand-in takes the call and
// records what would have been set.
func TestSetTimeEnablesThePrivilegeAndSetsUTC(t *testing.T) {
	var asked string
	var got foundation.SYSTEMTIME
	c := clock{
		enable: func(name string) error { asked = name; return nil },
		set:    func(st *foundation.SYSTEMTIME) error { got = *st; return nil },
	}
	// A zone east of UTC: setting its local time would put the clock ten
	// hours ahead.
	sydney := time.FixedZone("AEST", 10*60*60)
	if err := c.SetTime(
		context.Background(),
		time.Date(2026, 10, 3, 8, 30, 45, 123456789, sydney),
	); err != nil {
		t.Fatal(err)
	}
	want := foundation.SYSTEMTIME{
		WYear: 2026, WMonth: 10, WDayOfWeek: uint16(time.Friday), WDay: 2,
		WHour: 22, WMinute: 30, WSecond: 45, WMilliseconds: 123,
	}
	if asked != systemtimePrivilege || got != want {
		t.Errorf("enabled %q and set %+v, want %+v", asked, got, want)
	}
}

func TestSetTimeReportsEachFailure(t *testing.T) {
	errStep := errors.New("step failed")
	set := func(*foundation.SYSTEMTIME) error { t.Error("set without the privilege"); return nil }
	if err := (clock{enable: func(string) error { return errStep }, set: set}).SetTime(
		context.Background(),
		time.Now(),
	); !errors.Is(
		err,
		errStep,
	) {
		t.Errorf("enable failure: err = %v", err)
	}
	c := clock{
		enable: func(string) error { return nil },
		set:    func(*foundation.SYSTEMTIME) error { return errStep },
	}
	if err := c.SetTime(context.Background(), time.Now()); !errors.Is(err, errStep) {
		t.Errorf("set failure: err = %v", err)
	}
}
