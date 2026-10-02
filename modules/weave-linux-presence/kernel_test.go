//go:build linux

package main

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestKernelReadsTheRealSystem(t *testing.T) {
	k, err := newKernel().Kernel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if k == "" {
		t.Fatal("no kernel release for hello")
	}
	t.Logf("kernel = %q", k)
}

func TestKernelReportsAFailedUname(t *testing.T) {
	errUname := errors.New("uname failed")
	k := kernel{uname: func(*unix.Utsname) error { return errUname }}
	got, err := k.Kernel(context.Background())
	if !errors.Is(err, errUname) || got != "" {
		t.Fatalf("Kernel() = %q, %v; want empty and the uname error", got, err)
	}
}
