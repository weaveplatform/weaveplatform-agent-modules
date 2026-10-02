package guestwire

import (
	"slices"
	"strings"
)

// Capability names one guest feature. Each capability is served by its own
// module per guest OS (guestweave-linux-exec, guestweave-macos-exec,
// guestweave-windows-exec), and all of them answer to one OS-neutral channel
// address, so the host reaches "exec" without knowing which OS is behind it.
type Capability string

// Capabilities with a wire contract today.
const (
	Presence Capability = "presence"
	Exec     Capability = "exec"
	Power    Capability = "power"
	Time     Capability = "time"
	Metrics  Capability = "metrics"
)

// Reserved capabilities. The names are fixed now so no other module claims
// their addresses; their ops are defined here when their modules land, because
// an op without a payload schema is a contract nobody can implement.
const (
	Clipboard Capability = "clipboard"
	Network   Capability = "network"
	Files     Capability = "files"
	Shares    Capability = "shares"
	Tunnel    Capability = "tunnel"
	Provision Capability = "provision"
	Session   Capability = "session"
	Freeze    Capability = "freeze"
	Disk      Capability = "disk"
	Display   Capability = "display"
	Logs      Capability = "logs"
	Software  Capability = "software"
	Tools     Capability = "tools"
	Osquery   Capability = "osquery"
)

// Namespace prefixes every capability address and every op kind.
const Namespace = "guestweave"

// Address is the hypervisor-channel address a capability's modules answer to:
// the manifest `address` field, and the envelope Module the host writes.
func (c Capability) Address() string { return Namespace + "." + string(c) }

// Kind spells an op of this capability: guestweave.<capability>.<op>.
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
}

var reservedCapabilities = []Capability{
	Clipboard, Network, Files, Shares, Tunnel, Provision, Session,
	Freeze, Disk, Display, Logs, Software, Tools, Osquery,
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
// guestweave.<capability>.<op>[.result]. It reports false for anything else,
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
