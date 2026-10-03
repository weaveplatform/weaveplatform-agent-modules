//go:build linux

package main

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
)

// kernel supplies the kernel release for hello.
type kernel struct {
	uname func(*unix.Utsname) error
}

func newKernel() kernel { return kernel{uname: unix.Uname} }

// Kernel reports the kernel release from uname (e.g. "6.8.0-40-generic"). The
// service treats an error as an empty field; hello never waits on it.
func (k kernel) Kernel(context.Context) (string, error) {
	var u unix.Utsname
	if err := k.uname(&u); err != nil {
		return "", fmt.Errorf("uname: %w", err)
	}
	return unix.ByteSliceToString(u.Release[:]), nil
}
