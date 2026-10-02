//go:build unix

package weavepower

// macOS and Linux resolve to the same two commands by the same rule, so the
// backend lives here and both modules use it rather than keeping two copies
// that can drift. Windows has no equivalent — it goes through the Win32
// shutdown API in its own module.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Unix implements Backend for macOS and Linux guests.
type Unix struct{}

// Shutdown powers the guest off.
//
// systemd's own command is preferred where it exists: on a systemd guest
// /sbin/shutdown is a symlink into systemctl anyway, and calling systemctl
// directly avoids depending on that symlink being present. The fallback covers
// a non-systemd Linux and macOS, neither of which has systemctl at all.
func (Unix) Shutdown(ctx context.Context, _ string) (Action, error) {
	if systemctlAvailable() {
		return action(ctx, "systemctl", "poweroff"), nil
	}
	return action(ctx, "shutdown", "-h", "now"), nil
}

// Restart reboots the guest, by the same rule as Shutdown.
func (Unix) Restart(ctx context.Context, _ string) (Action, error) {
	if systemctlAvailable() {
		return action(ctx, "systemctl", "reboot"), nil
	}
	return action(ctx, "shutdown", "-r", "now"), nil
}

// The reason argument is accepted and ignored on Unix. Neither command has a
// field for it — Windows records one in its own shutdown log — and inventing a
// wall(1) message would put an operator-visible notice on screen that the host
// never asked for.

func action(ctx context.Context, name string, args ...string) Action {
	// Run happens after the reply is sent, when the request's context may be
	// gone; a shutdown must not be cancelled half-way, so only its values are
	// kept.
	runCtx := context.WithoutCancel(ctx)
	return Action{
		Command: strings.Join(append([]string{name}, args...), " "),
		Run: func() error {
			if err := exec.CommandContext(runCtx, name, args...).Run(); err != nil {
				return fmt.Errorf("weavepower: %s: %w", name, err)
			}
			return nil
		},
	}
}

// systemctlAvailable reports whether this guest has systemd's client on PATH.
// A variable so tests can drive both branches without a systemd guest.
var systemctlAvailable = func() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}
