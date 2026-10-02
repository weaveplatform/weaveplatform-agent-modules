package guestwire_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// The plan's capability table, spelled out once more on purpose: a capability
// renamed or dropped by accident changes an address every module and host
// depend on, and this is where that shows up.
func TestTheCapabilityVocabulary(t *testing.T) {
	implemented := []guestwire.Capability{"exec", "metrics", "power", "presence", "time"}
	reserved := []guestwire.Capability{
		"clipboard", "disk", "display", "files", "freeze", "logs", "network",
		"osquery", "provision", "session", "shares", "software", "tools", "tunnel",
	}
	if got := guestwire.Implemented(); !slices.Equal(got, implemented) {
		t.Fatalf("implemented = %v", got)
	}
	all := slices.Concat(implemented, reserved)
	slices.Sort(all)
	if got := guestwire.Capabilities(); !slices.Equal(got, all) {
		t.Fatalf("capabilities = %v", got)
	}
	for _, c := range reserved {
		if !c.Reserved() || !c.Valid() || c.Ops() != nil {
			t.Errorf("%s: reserved=%v valid=%v ops=%v", c, c.Reserved(), c.Valid(), c.Ops())
		}
	}
	for _, c := range implemented {
		if c.Reserved() || !c.Valid() || len(c.Ops()) == 0 {
			t.Errorf("%s: reserved=%v valid=%v ops=%v", c, c.Reserved(), c.Valid(), c.Ops())
		}
	}
	if guestwire.Capability("teleport").Valid() || guestwire.Capability("teleport").Reserved() {
		t.Fatal("an unknown capability was accepted")
	}
}

// Every op of a capability must be spelled under that capability's address,
// or core would route it to a module that does not serve it.
func TestOpsLiveUnderTheirCapabilitysAddress(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range guestwire.Implemented() {
		ops := c.Ops()
		if !slices.IsSorted(ops) {
			t.Errorf("%s ops not sorted: %v", c, ops)
		}
		for _, kind := range ops {
			if seen[kind] {
				t.Errorf("%s is claimed by two capabilities", kind)
			}
			seen[kind] = true
			if guestwire.IsResult(kind) {
				t.Errorf("%s is a reply kind", kind)
			}
			got, ok := guestwire.CapabilityOf(kind)
			if !ok || got != c {
				t.Errorf("CapabilityOf(%s) = %s, %v; want %s", kind, got, ok, c)
			}
			addr, ok := guestwire.AddressOf(kind)
			if !ok || addr != c.Address() || !strings.HasPrefix(kind, addr+".") {
				t.Errorf("AddressOf(%s) = %s, %v; want %s", kind, addr, ok, c.Address())
			}
		}
	}
	all := guestwire.AllCommandKinds()
	if len(all) != len(seen) || !slices.IsSorted(all) {
		t.Fatalf("AllCommandKinds = %v", all)
	}
	// Mutating a returned slice must not touch the contract.
	ops := guestwire.Exec.Ops()
	ops[0] = "mutated"
	if guestwire.Exec.Ops()[0] == "mutated" {
		t.Fatal("Ops exposed its backing array")
	}
}

func TestAddressing(t *testing.T) {
	if got := guestwire.Exec.Address(); got != "guestweave.exec" {
		t.Fatalf("address = %q", got)
	}
	if got := guestwire.Clipboard.Kind("get"); got != "guestweave.clipboard.get" {
		t.Fatalf("kind = %q", got)
	}
	for kind, want := range map[string]string{
		"guestweave.exec.start":        "guestweave.exec",
		"guestweave.exec.start.result": "guestweave.exec",
		"guestweave.future.op":         "guestweave.future", // forward compatible
		"guestweave.exec":              "",
		"guestweave..op":               "",
		"guestweave.exec.":             "",
		"sysinfo.collect":              "",
		"":                             "",
	} {
		got, ok := guestwire.AddressOf(kind)
		if got != want || ok != (want != "") {
			t.Errorf("AddressOf(%q) = %q, %v; want %q", kind, got, ok, want)
		}
	}
	// CapabilityOf is strict where AddressOf is not.
	if _, ok := guestwire.CapabilityOf("guestweave.future.op"); ok {
		t.Fatal("CapabilityOf accepted an unknown capability")
	}
	if _, ok := guestwire.CapabilityOf("nope"); ok {
		t.Fatal("CapabilityOf accepted a non-kind")
	}
	if c, ok := guestwire.CapabilityOf("guestweave.osquery.query"); !ok || c != guestwire.Osquery {
		t.Fatal("a reserved capability's kind was not recognised")
	}
}

func TestDeprecatedInventoryAlias(t *testing.T) {
	if guestwire.KindInventoryGet != guestwire.KindPresenceInventory { //nolint:staticcheck // the alias is what is under test
		t.Fatal("alias drifted")
	}
}

func FuzzAddressOf(f *testing.F) {
	for _, seed := range []string{"guestweave.exec.start", "guestweave..", "guestweave.a.b.c.result", "x"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, kind string) {
		addr, ok := guestwire.AddressOf(kind)
		if !ok {
			return
		}
		if !strings.HasPrefix(kind, addr+".") || strings.Count(addr, ".") != 1 {
			t.Fatalf("AddressOf(%q) = %q", kind, addr)
		}
		if c, ok := guestwire.CapabilityOf(kind); ok && c.Address() != addr {
			t.Fatalf("CapabilityOf and AddressOf disagree on %q", kind)
		}
	})
}
