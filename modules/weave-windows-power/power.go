//go:build windows

package main

import (
	"context"
	"fmt"
	"syscall"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/shutdown"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavepower"
)

// shutdownPrivilege is what InitiateShutdown needs, held by system and by
// administrators but disabled until enabled.
const shutdownPrivilege = "SeShutdownPrivilege"

// shutdownReason is recorded in the machine's own event log, so an operator
// inside it can tell a host-initiated shutdown from a local one: planned
// application maintenance.
const shutdownReason = shutdown.SHTDN_REASON_MAJOR_APPLICATION |
	shutdown.SHTDN_REASON_MINOR_MAINTENANCE |
	shutdown.SHTDN_REASON_FLAG_PLANNED

// shutdownFlags are common to both operations. FORCE_OTHERS closes
// applications rather than letting one with unsaved work veto the shutdown and
// leave the host waiting on a machine that will not stop. GRACE_OVERRIDE skips
// the warning period: the host has already decided, and a countdown nobody is
// there to see only delays it.
const shutdownFlags = shutdown.SHUTDOWN_FORCE_OTHERS | shutdown.SHUTDOWN_GRACE_OVERRIDE

// defaultMessage stands in for a request that gave no reason.
const defaultMessage = "Shutdown requested by the weave host"

// power asks Windows to shut down or restart through the Win32 shutdown API.
//
// Windows does not share the Unix backend in weavepower: it has a real API for
// this, and calling InitiateShutdown directly returns a real error code rather
// than a process exit status, without depending on shutdown.exe being present
// and on PATH in a locked-down image. It matters more here than anywhere: a
// Windows guest does not act on the ACPI power button, so without this path a
// host can only cut it off mid-write.
//
// Both steps are fields, so tests decide and "run" without powering off the
// machine running them.
type power struct {
	enable   func(name string) error
	initiate func(message string, flags shutdown.SHUTDOWN_FLAGS) uint32
}

func newPower() power {
	return power{
		enable: processPrivileges().enable,
		initiate: func(message string, flags shutdown.SHUTDOWN_FLAGS) uint32 {
			// Grace period 0: the delay is the host's to choose, and it chose
			// by asking.
			return shutdown.InitiateShutdown(nil, &message, 0, flags, shutdownReason)
		},
	}
}

// Shutdown powers the machine off.
func (p power) Shutdown(_ context.Context, reason string) (weavepower.Action, error) {
	return p.action("shutdown", shutdown.SHUTDOWN_POWEROFF, reason)
}

// Restart reboots the machine.
func (p power) Restart(_ context.Context, reason string) (weavepower.Action, error) {
	return p.action("restart", shutdown.SHUTDOWN_RESTART, reason)
}

func (p power) action(
	op string,
	mode shutdown.SHUTDOWN_FLAGS,
	reason string,
) (weavepower.Action, error) {
	// The privilege is enabled while deciding, not while acting. If this
	// machine cannot shut itself down the host must learn it in the reply it
	// is waiting on; once the reply is sent there is nobody left to tell, and
	// the host would wait for a power-off that is never coming.
	if err := p.enable(shutdownPrivilege); err != nil {
		return weavepower.Action{}, fmt.Errorf("weave power %s: %w", op, err)
	}
	flags := shutdownFlags | mode
	message := reason
	if message == "" {
		message = defaultMessage
	}
	return weavepower.Action{
		Command: fmt.Sprintf("InitiateShutdown(%s, flags=%#x)", op, uint32(flags)),
		Run: func() error {
			if rc := p.initiate(message, flags); rc != 0 {
				// syscall.Errno carries a Win32 error code and formats the
				// system's own message for it.
				return fmt.Errorf("InitiateShutdown: %w", syscall.Errno(rc))
			}
			return nil
		},
	}, nil
}
