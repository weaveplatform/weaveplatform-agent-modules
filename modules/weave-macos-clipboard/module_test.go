//go:build darwin

package main

import (
	"context"
	"slices"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/manifest"
	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

const wantID = "weave-macos-clipboard"

func TestMainServesTheClipboardModule(t *testing.T) {
	old := serve
	t.Cleanup(func() { serve = old })
	var got modulesdk.Module
	serve = func(m modulesdk.Module) { got = m }
	main()
	if got == nil || got.ID() != wantID {
		t.Fatalf("main served %v, want %s", got, wantID)
	}
}

// Parity: the module serves exactly clipboard's ops, with its real backends.
func TestParity(t *testing.T) {
	if err := weavemodule.CheckParity(newService()); err != nil {
		t.Fatal(err)
	}
}

func loadManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Load("module.manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The manifest is what core routes and gates on, so everything it declares
// must agree with what the binary does.
func TestManifest(t *testing.T) {
	m := loadManifest(t)
	if err := weavemodule.CheckManifest(m, newService()); err != nil {
		t.Fatal(err)
	}
	mod := newModule()
	if m.ID != mod.ID() {
		t.Errorf("manifest id %q, module id %q", m.ID, mod.ID())
	}
	if m.ChannelAddress() != weavewire.Clipboard.Address() || mod.Address() != m.ChannelAddress() {
		t.Errorf("manifest address %q, module address %q", m.ChannelAddress(), mod.Address())
	}
	// Core gates launch on the manifest's capabilities and the module asks for
	// its Requires at Init; a mismatch is a module core never starts.
	var requires []string
	for _, c := range mod.Requires() {
		requires = append(requires, string(c))
	}
	if !slices.Equal(m.Capabilities, requires) {
		t.Errorf("manifest capabilities %v, module requires %v", m.Capabilities, requires)
	}
	// macOS guests run only on Apple silicon (Virtualization.framework has no
	// Intel macOS guest), so there is no darwin/amd64 build to ship.
	if len(m.Platforms) != 1 || !m.SupportsHost("darwin", "arm64") {
		t.Errorf("platforms = %v, want darwin/arm64 alone", m.Platforms)
	}
	// The pasteboard is the console user's: core starts the module as that
	// user in their GUI session's bootstrap (gui/<uid>). As system it would
	// reach no pasteboard server, and core refuses per-user-console with any
	// privilege but user.
	if m.Privilege != manifest.PrivilegeUser || m.Session != manifest.SessionPerUserConsole {
		t.Errorf("privilege %q session %q, want user/per-user-console", m.Privilege, m.Session)
	}
	if m.Zone != "A" {
		t.Errorf("zone %q, want A: the module is pure Go", m.Zone)
	}
}

func TestModuleIsHealthyWithoutAReport(t *testing.T) {
	if h := newModule().Health(); h.Status != modulesdk.HealthHealthy {
		t.Fatalf("health = %v", h)
	}
	if err := newModule().Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
