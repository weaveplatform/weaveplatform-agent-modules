//go:build unix

package weavepower

import (
	"context"
	"testing"
)

func withSystemctl(t *testing.T, present bool) {
	t.Helper()
	orig := systemctlAvailable
	systemctlAvailable = func() bool { return present }
	t.Cleanup(func() { systemctlAvailable = orig })
}

func TestCommandSelection(t *testing.T) {
	tests := []struct {
		name      string
		systemctl bool
		restart   bool
		want      string
	}{
		{"systemd shutdown", true, false, "systemctl poweroff"},
		{"systemd restart", true, true, "systemctl reboot"},
		{"traditional shutdown", false, false, "shutdown -h now"},
		{"traditional restart", false, true, "shutdown -r now"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withSystemctl(t, tc.systemctl)
			var (
				act Action
				err error
			)
			if tc.restart {
				act, err = Unix{}.Restart(context.Background(), "because")
			} else {
				act, err = Unix{}.Shutdown(context.Background(), "because")
			}
			if err != nil {
				t.Fatal(err)
			}
			if act.Command != tc.want {
				t.Fatalf("command = %q, want %q", act.Command, tc.want)
			}
			// Deciding must not act: the whole ordering contract depends on
			// Run being a closure the dispatcher calls later, never something
			// that already happened by the time we return.
			if act.Run == nil {
				t.Fatal("no Run closure — nothing would happen after the reply")
			}
		})
	}
}

// Run must execute exactly the command Command describes; a harmless one
// stands in for shutdown.
func TestActionRunsTheCommandItDescribes(t *testing.T) {
	act := action(context.Background(), "true")
	if act.Command != "true" {
		t.Fatalf("command = %q", act.Command)
	}
	if err := act.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := action(context.Background(), "false").Run(); err == nil {
		t.Fatal("a failing command reported success")
	}
}

func TestSystemctlProbeAnswers(t *testing.T) {
	// The answer depends on the machine; what matters is that probing works
	// without panicking on a host with or without systemd.
	_ = systemctlAvailable()
}
