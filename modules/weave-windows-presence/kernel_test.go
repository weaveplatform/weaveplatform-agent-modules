//go:build windows

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestKernelReadsTheRealSystem(t *testing.T) {
	k, err := newKernel().Kernel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(k, ".") != 2 {
		t.Fatalf("kernel = %q, want major.minor.build", k)
	}
	t.Logf("kernel = %q", k)
}

func TestKernelReportsAnEmptyVersion(t *testing.T) {
	k := kernel{version: func() (string, string) { return "", "22631" }}
	if got, err := k.Kernel(context.Background()); !errors.Is(err, errNoVersion) || got != "" {
		t.Fatalf("Kernel() = %q, %v; want empty and errNoVersion", got, err)
	}
}
