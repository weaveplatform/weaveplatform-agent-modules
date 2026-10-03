//go:build linux

package main

import (
	"context"
	"slices"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/manifest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

const wantID = "weave-linux-clipboard"

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
	for _, arch := range []string{"amd64", "arm64"} {
		if !m.SupportsHost("linux", arch) {
			t.Errorf("manifest does not list linux/%s", arch)
		}
	}
	// The clipboard is the console user's: core starts the module as that
	// user inside their session, with the session's display in its
	// environment. As system it would find no display server to talk to, and
	// core refuses per-user-console with any privilege but user.
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
