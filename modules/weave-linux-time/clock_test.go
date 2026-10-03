//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The clock is never actually set by a test: a stand-in takes the call and
// records what would have been set.
func TestSetTimeStepsTheRealtimeClock(t *testing.T) {
	var gotID int32
	var got unix.Timespec
	c := clock{
		settime: func(id int32, ts *unix.Timespec) error { gotID, got = id, *ts; return nil },
	}
	want := time.Date(2026, 10, 2, 12, 30, 45, 123456789, time.UTC)
	if err := c.SetTime(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if gotID != unix.CLOCK_REALTIME || time.Unix(got.Unix()).UTC() != want {
		t.Errorf(
			"set clock %d to %v, want CLOCK_REALTIME at %v",
			gotID,
			time.Unix(got.Unix()).UTC(),
			want,
		)
	}
}

func TestSetTimeReportsTheKernelsRefusal(t *testing.T) {
	c := clock{settime: func(int32, *unix.Timespec) error { return unix.EPERM }}
	if err := c.SetTime(context.Background(), time.Now()); !errors.Is(err, unix.EPERM) {
		t.Fatalf("err = %v, want EPERM", err)
	}
}

// The real syscall, where it cannot succeed: without CAP_SYS_TIME the kernel
// refuses, which proves the call is wired without moving the clock. As root it
// would succeed, so it is skipped there.
func TestSetTimeWithoutPrivilegeIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the real call would set the clock")
	}
	if err := newClock().SetTime(context.Background(), time.Now()); !errors.Is(err, unix.EPERM) {
		t.Fatalf("err = %v, want EPERM for an unprivileged caller", err)
	}
}
