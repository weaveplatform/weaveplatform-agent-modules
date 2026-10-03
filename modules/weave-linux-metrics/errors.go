//go:build linux

package main

import "errors"

// errProcFormat reports a /proc file that does not read the way the kernel
// documents it.
var errProcFormat = errors.New("unexpected /proc format")
