//go:build darwin

package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// The clock is never actually set by a test: a stand-in takes the call and
// records what would have been set.
func TestSetTimeStepsTheClock(t *testing.T) {
	var got syscall.Timeval
	c := clock{settimeofday: func(tv *syscall.Timeval) error { got = *tv; return nil }}
	want := time.Date(2026, 10, 2, 12, 30, 45, 123456000, time.UTC)
	if err := c.SetTime(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if set := time.Unix(got.Unix()).UTC(); !set.Equal(want) {
		t.Errorf("set the clock to %v, want %v", set, want)
	}
}

func TestSetTimeReportsTheKernelsRefusal(t *testing.T) {
	c := clock{settimeofday: func(*syscall.Timeval) error { return syscall.EPERM }}
	if err := c.SetTime(context.Background(), time.Now()); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("err = %v, want EPERM", err)
	}
}

// The real syscall, where it cannot succeed: as a user the kernel refuses,
// which proves the call is wired without moving the clock. As root it would
// succeed, so it is skipped there.
func TestSetTimeWithoutPrivilegeIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the real call would set the clock")
	}
	if err := newClock().SetTime(context.Background(), time.Now()); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("err = %v, want EPERM for an unprivileged caller", err)
	}
}
