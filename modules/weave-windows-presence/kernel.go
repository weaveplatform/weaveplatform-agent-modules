//go:build windows

package main

import (
	"context"
	"errors"
)

// kernel supplies the Windows version for hello, read from the registry (see
// windowsVersion) rather than GetVersionEx, which reports a stale version to a
// binary without an application manifest.
type kernel struct {
	version func() (version, build string)
}

func newKernel() kernel {
	return kernel{version: func() (string, string) { return windowsVersion(regString, regDWORD) }}
}

var errNoVersion = errors.New("the registry reported no Windows version")

// Kernel reports the Windows version (e.g. "10.0.22631"). The service treats
// an error as an empty field; hello never waits on it.
func (k kernel) Kernel(context.Context) (string, error) {
	v, _ := k.version()
	if v == "" {
		return "", errNoVersion
	}
	return v, nil
}
