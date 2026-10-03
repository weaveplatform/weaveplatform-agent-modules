package weavewire_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// The plan's capability table, spelled out once more on purpose: a capability
// renamed or dropped by accident changes an address every module and host
// depend on, and this is where that shows up.
func TestTheCapabilityVocabulary(t *testing.T) {
	implemented := []weavewire.Capability{
		"clipboard", "display", "exec", "metrics", "power", "presence", "session", "time",
	}
	reserved := []weavewire.Capability{
		"disk", "files", "freeze", "logs", "network",
		"osquery", "provision", "shares", "software", "tools", "tunnel",
	}
	if got := weavewire.Implemented(); !slices.Equal(got, implemented) {
		t.Fatalf("implemented = %v", got)
	}
	all := slices.Concat(implemented, reserved)
	slices.Sort(all)
	if got := weavewire.Capabilities(); !slices.Equal(got, all) {
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
	if weavewire.Capability("teleport").Valid() || weavewire.Capability("teleport").Reserved() {
		t.Fatal("an unknown capability was accepted")
	}
}

// Every op of a capability must be spelled under that capability's address,
// or core would route it to a module that does not serve it.
func TestOpsLiveUnderTheirCapabilitysAddress(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range weavewire.Implemented() {
		ops := c.Ops()
		if !slices.IsSorted(ops) {
			t.Errorf("%s ops not sorted: %v", c, ops)
		}
		for _, kind := range ops {
			if seen[kind] {
				t.Errorf("%s is claimed by two capabilities", kind)
			}
			seen[kind] = true
			if weavewire.IsResult(kind) {
				t.Errorf("%s is a reply kind", kind)
			}
			got, ok := weavewire.CapabilityOf(kind)
			if !ok || got != c {
				t.Errorf("CapabilityOf(%s) = %s, %v; want %s", kind, got, ok, c)
			}
			addr, ok := weavewire.AddressOf(kind)
			if !ok || addr != c.Address() || !strings.HasPrefix(kind, addr+".") {
				t.Errorf("AddressOf(%s) = %s, %v; want %s", kind, addr, ok, c.Address())
			}
		}
	}
	all := weavewire.AllCommandKinds()
	if len(all) != len(seen) || !slices.IsSorted(all) {
		t.Fatalf("AllCommandKinds = %v", all)
	}
	// Mutating a returned slice must not touch the contract.
	ops := weavewire.Exec.Ops()
	ops[0] = "mutated"
	if weavewire.Exec.Ops()[0] == "mutated" {
		t.Fatal("Ops exposed its backing array")
	}
}

func TestAddressing(t *testing.T) {
	if got := weavewire.Exec.Address(); got != "weave.exec" {
		t.Fatalf("address = %q", got)
	}
	if got := weavewire.Clipboard.Kind("get"); got != "weave.clipboard.get" {
		t.Fatalf("kind = %q", got)
	}
	for kind, want := range map[string]string{
		"weave.exec.start":        "weave.exec",
		"weave.exec.start.result": "weave.exec",
		"weave.future.op":         "weave.future", // forward compatible
		"weave.exec":              "",
		"weave..op":               "",
		"weave.exec.":             "",
		"sysinfo.collect":         "",
		"":                        "",
	} {
		got, ok := weavewire.AddressOf(kind)
		if got != want || ok != (want != "") {
			t.Errorf("AddressOf(%q) = %q, %v; want %q", kind, got, ok, want)
		}
	}
	// CapabilityOf is strict where AddressOf is not.
	if _, ok := weavewire.CapabilityOf("weave.future.op"); ok {
		t.Fatal("CapabilityOf accepted an unknown capability")
	}
	if _, ok := weavewire.CapabilityOf("nope"); ok {
		t.Fatal("CapabilityOf accepted a non-kind")
	}
	if c, ok := weavewire.CapabilityOf("weave.osquery.query"); !ok || c != weavewire.Osquery {
		t.Fatal("a reserved capability's kind was not recognised")
	}
}

func TestDeprecatedInventoryAlias(t *testing.T) {
	if weavewire.KindInventoryGet != weavewire.KindPresenceInventory { //nolint:staticcheck // the alias is what is under test
		t.Fatal("alias drifted")
	}
}

func FuzzAddressOf(f *testing.F) {
	for _, seed := range []string{"weave.exec.start", "weave..", "weave.a.b.c.result", "x"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, kind string) {
		addr, ok := weavewire.AddressOf(kind)
		if !ok {
			return
		}
		if !strings.HasPrefix(kind, addr+".") || strings.Count(addr, ".") != 1 {
			t.Fatalf("AddressOf(%q) = %q", kind, addr)
		}
		if c, ok := weavewire.CapabilityOf(kind); ok && c.Address() != addr {
			t.Fatalf("CapabilityOf and AddressOf disagree on %q", kind)
		}
	})
}

// The ops of each capability, spelled out: they are what every module serves
// and every host sends, and a kind renamed by accident is a command that goes
// unanswered.
func TestTheOpsOfEachCapability(t *testing.T) {
	for c, want := range map[weavewire.Capability][]string{
		weavewire.Clipboard: {
			"weave.clipboard.get", "weave.clipboard.set",
			"weave.clipboard.stat", "weave.clipboard.upload",
		},
		weavewire.Session:  {"weave.session.current", "weave.session.list", "weave.session.lock"},
		weavewire.Display:  {"weave.display.list", "weave.display.set"},
		weavewire.Presence: {"weave.presence.hello", "weave.presence.inventory"},
		weavewire.Exec: {
			"weave.exec.resize", "weave.exec.signal", "weave.exec.start", "weave.exec.stdin",
		},
		weavewire.Power:   {"weave.power.restart", "weave.power.shutdown"},
		weavewire.Time:    {"weave.time.get", "weave.time.set"},
		weavewire.Metrics: {"weave.metrics.sample"},
	} {
		if got := c.Ops(); !slices.Equal(got, want) {
			t.Errorf("%s ops = %v, want %v", c, got, want)
		}
	}
	// Events are guest-to-host, so they are never ops a module serves.
	for _, event := range []string{
		weavewire.KindClipboardDownload, weavewire.KindSessionChanged,
		weavewire.KindExecStdout, weavewire.KindExecExit,
	} {
		if slices.Contains(weavewire.AllCommandKinds(), event) {
			t.Errorf("event %s is listed as a command", event)
		}
		if c, ok := weavewire.CapabilityOf(event); !ok || !slices.Contains(weavewire.Implemented(), c) {
			t.Errorf("event %s is not under an implemented capability", event)
		}
	}
}

// Clipboard and display only exist in the console user's session; everything
// else runs as system. A host's no-session handling and every module's
// manifest check key off this.
func TestPlacement(t *testing.T) {
	for _, c := range weavewire.Capabilities() {
		want := weavewire.PlacementSystem
		if c == weavewire.Clipboard || c == weavewire.Display {
			want = weavewire.PlacementPerUserConsole
		}
		if got := c.Placement(); got != want {
			t.Errorf("%s placement = %q, want %q", c, got, want)
		}
	}
}
