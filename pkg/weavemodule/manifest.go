package weavemodule

import (
	"errors"
	"fmt"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/manifest"
)

// guestOS maps the OS segment of a module id to the GOOS its manifest must
// list. Each variant is built for exactly one guest OS.
var guestOS = map[string]string{"linux": "linux", "macos": "darwin", "windows": "windows"}

// CheckManifest reports whether a module's manifest declares what core needs
// to route the host's commands to svc: a valid manifest whose channel address
// is the capability's, whose session is the capability's placement, whose id
// is weave-<os>-<capability>, and whose platforms are that one OS.
//
// The address is the part that fails silently. Core routes by
// Manifest.ChannelAddress(), so a manifest that omits address answers to its
// id, and every host command for the capability goes unanswered.
func CheckManifest(m *manifest.Manifest, svc Service) error {
	if err := m.Validate(); err != nil {
		return fmt.Errorf("%w: %w", errManifest, err)
	}
	c := svc.Capability()
	if got, want := m.ChannelAddress(), c.Address(); got != want {
		return fmt.Errorf(
			"%w: %s answers to %q, the capability's address is %q",
			errManifest,
			m.ID,
			got,
			want,
		)
	}
	// Core starts a module where its manifest says. A clipboard declared as
	// system would run in session 0 or the wrong bootstrap and find no
	// clipboard; a power module declared per-user-console would wait for a
	// login it does not need.
	if got, want := m.Session, c.Placement(); got != want {
		return fmt.Errorf(
			"%w: %s declares session %q, %s runs in %q",
			errManifest,
			m.ID,
			got,
			c,
			want,
		)
	}
	var goos string
	for os, g := range guestOS {
		if m.ID == ModuleID(os, c) {
			goos = g
		}
	}
	if goos == "" {
		return fmt.Errorf(
			"%w: id %q is not %s for linux, macos or windows",
			errManifest,
			m.ID,
			ModuleID("<os>", c),
		)
	}
	for _, p := range m.Platforms {
		if p.OS != goos {
			return fmt.Errorf("%w: %s lists platform %s/%s", errManifest, m.ID, p.OS, p.Arch)
		}
	}
	return nil
}

var errManifest = errors.New("weavemodule: manifest")
