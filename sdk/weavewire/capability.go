package weavewire

import (
	"slices"
	"strings"
)

// Capability names one guest feature. Each capability is served by its own
// module per guest OS (weave-linux-exec, weave-macos-exec,
// weave-windows-exec), and all of them answer to one OS-neutral channel
// address, so the host reaches "exec" without knowing which OS is behind it.
type Capability string

// Capabilities with a wire contract today.
const (
	Presence  Capability = "presence"
	Exec      Capability = "exec"
	Power     Capability = "power"
	Time      Capability = "time"
	Metrics   Capability = "metrics"
	Clipboard Capability = "clipboard"
	Session   Capability = "session"
	Display   Capability = "display"
)

// Reserved capabilities. The names are fixed now so no other module claims
// their addresses; their ops are defined here when their modules land, because
// an op without a payload schema is a contract nobody can implement.
const (
	Network   Capability = "network"
	Files     Capability = "files"
	Shares    Capability = "shares"
	Tunnel    Capability = "tunnel"
	Provision Capability = "provision"
	Freeze    Capability = "freeze"
	Disk      Capability = "disk"
	Logs      Capability = "logs"
	Software  Capability = "software"
	Tools     Capability = "tools"
	Osquery   Capability = "osquery"
)

// Namespace prefixes every capability address and every op kind.
const Namespace = "weave"

// Address is the hypervisor-channel address a capability's modules answer to:
// the manifest `address` field, and the envelope Module the host writes.
func (c Capability) Address() string { return Namespace + "." + string(c) }

// Kind spells an op of this capability: weave.<capability>.<op>.
func (c Capability) Kind(op string) string { return c.Address() + "." + op }

// Ops is the sorted list of command kinds a module for this capability must
// serve. It is nil for a reserved capability.
//
// This is the parity contract: every OS variant of a capability asserts it
// serves exactly these, so an op implemented on two OSes and forgotten on the
// third fails that module's build instead of going unanswered in one guest.
func (c Capability) Ops() []string {
	return slices.Clone(capabilityOps[c])
}

// Reserved reports whether c is named but has no ops yet.
func (c Capability) Reserved() bool {
	_, known := capabilityOps[c]
	return !known && slices.Contains(reservedCapabilities, c)
}

// Valid reports whether c is a capability this vocabulary knows, implemented
// or reserved.
func (c Capability) Valid() bool {
	_, known := capabilityOps[c]
	return known || slices.Contains(reservedCapabilities, c)
}

var capabilityOps = map[Capability][]string{
	Presence: sorted(KindPresenceHello, KindPresenceInventory),
	Exec:     sorted(KindExecStart, KindExecStdin, KindExecResize, KindExecSignal),
	Power:    sorted(KindPowerShutdown, KindPowerRestart),
	Time:     sorted(KindTimeGet, KindTimeSet),
	Metrics:  sorted(KindMetricsSample),
	Clipboard: sorted(
		KindClipboardStat, KindClipboardGet, KindClipboardSet, KindClipboardUpload,
		KindClipboardFetch, KindClipboardStage, KindClipboardPut, KindClipboardCredit,
		KindClipboardCancel,
	),
	Session: sorted(KindSessionCurrent, KindSessionList, KindSessionLock),
	Display: sorted(KindDisplayList, KindDisplaySet),
}

var reservedCapabilities = []Capability{
	Network, Files, Shares, Tunnel, Provision,
	Freeze, Disk, Logs, Software, Tools, Osquery,
}

// Where a capability's module runs, as its manifest's `session` declares it.
// The values are agent-core's manifest session names.
const (
	// PlacementSystem is core's own session, as root or the service account.
	PlacementSystem = "system"
	// PlacementPerUserConsole is the session of the user at the physical
	// console, as that user. Core starts such a module only while someone is
	// logged in there and holds it in waiting-for-session otherwise, so a
	// command for it can go unanswered on a perfectly healthy machine.
	PlacementPerUserConsole = "per-user-console"
)

// perUserConsole lists the capabilities that only exist inside the console
// user's session: the clipboard and the display belong to a desktop, and a
// system process sees neither (session 0 on Windows, the wrong bootstrap on
// macOS, no compositor socket on Linux).
var perUserConsole = []Capability{Clipboard, Display}

// Placement is the session the capability's modules run in: PlacementSystem or
// PlacementPerUserConsole. Every OS variant declares the same one, and a host
// uses it to tell "nobody is logged in" from "no agent" when a command goes
// unanswered.
func (c Capability) Placement() string {
	if slices.Contains(perUserConsole, c) {
		return PlacementPerUserConsole
	}
	return PlacementSystem
}

func sorted(kinds ...string) []string {
	slices.Sort(kinds)
	return kinds
}

// Capabilities returns every capability, implemented and reserved, sorted.
func Capabilities() []Capability {
	out := make([]Capability, 0, len(capabilityOps)+len(reservedCapabilities))
	for c := range capabilityOps {
		out = append(out, c)
	}
	out = append(out, reservedCapabilities...)
	slices.Sort(out)
	return out
}

// Implemented returns the capabilities that have ops, sorted.
func Implemented() []Capability {
	out := make([]Capability, 0, len(capabilityOps))
	for c := range capabilityOps {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// CapabilityOf extracts the capability from a kind of the form
// weave.<capability>.<op>[.result]. It reports false for anything else,
// including a kind naming a capability this vocabulary does not know.
func CapabilityOf(kind string) (Capability, bool) {
	addr, ok := AddressOf(kind)
	if !ok {
		return "", false
	}
	c := Capability(strings.TrimPrefix(addr, Namespace+"."))
	return c, c.Valid()
}

// AddressOf is the channel address a kind travels under: the address of the
// capability it belongs to. It is how the host addresses a command and how it
// checks that a reply came from the module it asked.
//
// Unlike CapabilityOf it is purely syntactic, so a host built against this
// vocabulary can still address a capability added after it was released.
func AddressOf(kind string) (string, bool) {
	rest, ok := strings.CutPrefix(kind, Namespace+".")
	if !ok {
		return "", false
	}
	name, op, ok := strings.Cut(rest, ".")
	if !ok || name == "" || op == "" {
		return "", false
	}
	return Namespace + "." + name, true
}
