//go:build windows

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

const wantID = "weave-windows-session"

func TestMainServesTheSessionModule(t *testing.T) {
	old := serve
	t.Cleanup(func() { serve = old })
	var got modulesdk.Module
	serve = func(m modulesdk.Module) { got = m }
	main()
	if got == nil || got.ID() != wantID {
		t.Fatalf("main served %v, want %s", got, wantID)
	}
}

// Parity: the module serves exactly session's ops, with its real backends.
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
	if m.ChannelAddress() != weavewire.Session.Address() || mod.Address() != m.ChannelAddress() {
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
		if !m.SupportsHost("windows", arch) {
			t.Errorf("manifest does not list windows/%s", arch)
		}
	}
	// The service privilege (core's restricted token, every privilege
	// removed), not system: reading sessions through the Terminal Services
	// API is governed by the sessions' ACLs, which grant the system account
	// query access whatever privileges its token holds, and lock is
	// unsupported on Windows, so no op needs a privilege. Session placement
	// because the capability must answer with nobody logged in — that is
	// its point.
	if m.Privilege != manifest.PrivilegeService || m.Session != manifest.SessionSystem {
		t.Errorf("privilege %q session %q, want service/system", m.Privilege, m.Session)
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

// Core's release verifier runs WinVerifyTrust over a Windows module and then
// pins the leaf certificate's SHA-1 thumbprint to the manifest's: the
// weaveplatform code-signing certificate module-release.yml signs with. Core
// releases up to 0.9.8 also refuse a manifest with no authenticode_subject
// before they read the thumbprint, so the subject (the certificate's CN) is
// pinned too; the thumbprint, when present, is what is compared.
func TestManifestPinsTheReleaseCertificate(t *testing.T) {
	m := loadManifest(t)
	if m.Signing == nil ||
		m.Signing.AuthenticodeThumbprint != "A6A3936288B9409ED7A3458CF81014A77AB59B51" ||
		m.Signing.AuthenticodeSubject != "weaveplatform code signing" {
		t.Errorf("signing %+v, want authenticode_thumbprint A6A3936288B9409ED7A3458CF81014A77AB59B51"+
			" and authenticode_subject \"weaveplatform code signing\"", m.Signing)
	}
}
