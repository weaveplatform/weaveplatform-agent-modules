//go:build darwin

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Through the real binding: if purego failed to load Foundation this comes
// back empty, which is the regression worth catching.
func TestKernelReadsTheRealSystem(t *testing.T) {
	k, err := newKernel().Kernel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k, "Version ") {
		t.Fatalf("kernel = %q, want NSProcessInfo's \"Version N (Build B)\"", k)
	}
	t.Logf("kernel = %q", k)
}

func TestKernelReportsAnEmptyVersion(t *testing.T) {
	k := kernel{versionString: func() string { return "" }}
	if got, err := k.Kernel(context.Background()); !errors.Is(err, errNoVersion) || got != "" {
		t.Fatalf("Kernel() = %q, %v; want empty and errNoVersion", got, err)
	}
}
