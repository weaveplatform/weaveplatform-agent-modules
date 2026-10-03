//go:build darwin

package main

import (
	"context"
	"errors"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/foundation"
)

// kernel supplies the OS string for hello through the house binding
// (Foundation's NSProcessInfo), which is pure Go on purego, so the module stays
// CGO_ENABLED=0.
//
// It reports the OS an operator recognises ("Version 26.5.1 (Build 25F80)")
// rather than kern.osrelease's Darwin number (25.5.0), which a host would only
// have to translate back.
type kernel struct {
	versionString func() string
}

func newKernel() kernel {
	return kernel{versionString: func() string {
		return foundation.NSProcessInfoProcessInfo().OperatingSystemVersionString()
	}}
}

var errNoVersion = errors.New("NSProcessInfo reported no operating system version")

// Kernel reports the macOS version and build. The service treats an error as
// an empty field; hello never waits on it.
func (k kernel) Kernel(context.Context) (string, error) {
	v := k.versionString()
	if v == "" {
		return "", errNoVersion
	}
	return v, nil
}
