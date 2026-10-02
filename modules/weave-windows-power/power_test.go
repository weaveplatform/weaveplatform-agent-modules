//go:build windows

package main

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/shutdown"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavepower"
)

const wantPrivilege = shutdownPrivilege

type initiated struct {
	message string
	flags   shutdown.SHUTDOWN_FLAGS
}

// recorder is the real decision with the shutdown itself replaced: no test
// powers off the machine running it.
func recorder(rc uint32) (power, *[]initiated) {
	var calls []initiated
	return power{
		enable: func(string) error { return nil },
		initiate: func(message string, flags shutdown.SHUTDOWN_FLAGS) uint32 {
			calls = append(calls, initiated{message, flags})
			return rc
		},
	}, &calls
}

func TestShutdownAndRestartDecideThenRun(t *testing.T) {
	p, calls := recorder(0)
	for _, tc := range []struct {
		op     string
		decide func(context.Context, string) (weavepower.Action, error)
		mode   shutdown.SHUTDOWN_FLAGS
	}{
		{"shutdown", p.Shutdown, shutdown.SHUTDOWN_POWEROFF},
		{"restart", p.Restart, shutdown.SHUTDOWN_RESTART},
	} {
		a, err := tc.decide(context.Background(), "maintenance window")
		if err != nil {
			t.Fatalf("%s: %v", tc.op, err)
		}
		if len(*calls) != 0 {
			t.Fatalf("%s ran while deciding; it must wait for the reply to be sent", tc.op)
		}
		if !strings.Contains(a.Command, tc.op) {
			t.Errorf("%s command = %q", tc.op, a.Command)
		}
		if err := a.Run(); err != nil {
			t.Fatalf("%s run: %v", tc.op, err)
		}
		want := initiated{"maintenance window", shutdownFlags | tc.mode}
		if len(*calls) != 1 || (*calls)[0] != want {
			t.Errorf("%s initiated %+v, want %+v", tc.op, *calls, want)
		}
		*calls = nil
	}
}

func TestShutdownWithoutAReasonSaysWhoAsked(t *testing.T) {
	p, calls := recorder(0)
	a, err := p.Shutdown(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Run(); err != nil {
		t.Fatal(err)
	}
	if (*calls)[0].message != defaultMessage {
		t.Errorf("message = %q", (*calls)[0].message)
	}
}

// A machine that cannot shut itself down says so in the reply, before anything
// is acknowledged.
func TestShutdownWithoutThePrivilegeIsRefusedUpFront(t *testing.T) {
	p, calls := recorder(0)
	var asked string
	p.enable = func(name string) error { asked = name; return errNotHeld }
	if _, err := p.Restart(context.Background(), ""); !errors.Is(err, errNotHeld) {
		t.Fatalf("err = %v, want errNotHeld", err)
	}
	if asked != shutdownPrivilege || len(*calls) != 0 {
		t.Errorf("asked for %q and initiated %v", asked, *calls)
	}
}

func TestAFailedShutdownCarriesTheWin32Error(t *testing.T) {
	const errorAccessDenied = 5
	p, _ := recorder(errorAccessDenied)
	a, err := p.Shutdown(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Run(); !errors.Is(err, syscall.Errno(errorAccessDenied)) {
		t.Fatalf("err = %v, want ERROR_ACCESS_DENIED", err)
	}
}

// The real backend's decision, which enables the privilege in this process
// only; its action is never run.
func TestTheRealBackendDecides(t *testing.T) {
	a, err := newPower().Shutdown(context.Background(), "")
	if errors.Is(err, errNotHeld) {
		t.Skipf("the runner's account does not hold %s", shutdownPrivilege)
	}
	if err != nil {
		t.Fatal(err)
	}
	if a.Run == nil || !strings.HasPrefix(a.Command, "InitiateShutdown(shutdown") {
		t.Errorf("action = %+v", a)
	}
}
