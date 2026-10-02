// Package weavewire is the OS-agnostic wire vocabulary every weave
// capability module and the host client both speak. It owns the capability
// names and their channel addresses, the command kind strings, the correlation
// envelope, and the per-op request/response payloads — nothing OS-specific.
// Both ends import this one package so they cannot drift: a host that asks for
// weave.power.shutdown and a guest that answers a differently-spelled kind
// would silently never talk.
//
// Commands ride core's hypervisor channel as an hvchannel.Envelope whose Module
// is the capability's address (weave.<capability>) and whose Kind is the
// op (weave.<capability>.<op>). Data carries a Command (a correlation id
// plus an opaque per-op payload). The guest replies with the same Kind +
// ".result" and a Result echoing the id. Core never interprets Data — this
// vocabulary is product logic, deliberately kept out of the platform API.
package weavewire

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ProtocolVersion is reported on hello so a host can tell which wire contract
// the guest speaks. Bump it on any change a host has to know about.
//
// 2 is the capability-addressed contract: one module per capability, each
// under its own address, with inventory moved under presence.
const ProtocolVersion = "weave/1"

// Presence and power command kinds. The reply kind is the command kind +
// ".result" (see ResultKind).
const (
	// KindPresenceHello asks the guest to identify itself. It is the one op
	// core answers before the channel authenticates, and core matches it by
	// this exact spelling (hvchannel.PreAuthKind), so it must never change.
	KindPresenceHello = "weave.presence.hello"
	// KindPresenceInventory asks for the guest's hardware/OS facts and its
	// live network interfaces — the addresses in particular, which the host
	// cannot see from outside once a guest is on anything but a host-managed
	// network.
	KindPresenceInventory = "weave.presence.inventory"
	// KindPowerShutdown / KindPowerRestart ask the guest OS to power off or
	// reboot cleanly (the inside-OS half the hypervisor cannot do).
	KindPowerShutdown = "weave.power.shutdown"
	KindPowerRestart  = "weave.power.restart"
)

// KindInventoryGet is the old name of KindPresenceInventory.
//
// Deprecated: use KindPresenceInventory. Inventory is a presence op now that
// each capability has its own address.
const KindInventoryGet = KindPresenceInventory

// AllCommandKinds is every command kind of every implemented capability,
// sorted. A single module serves only its own capability's Ops; this is for
// hosts and tests that reason about the whole vocabulary.
func AllCommandKinds() []string {
	var kinds []string
	for _, c := range Implemented() {
		kinds = append(kinds, c.Ops()...)
	}
	slices.Sort(kinds)
	return kinds
}

const resultSuffix = ".result"

// ResultKind is the reply kind for a command kind.
func ResultKind(kind string) string { return kind + resultSuffix }

// MaxChunkBytes caps one stream chunk's payload. Chunks ride the module's gRPC
// stream to core before reaching the wire, and gRPC's default receive limit is
// 4 MiB, so this stays an order of magnitude below it: a stream that dies at
// the transport because one chunk was too big is far worse than one that takes
// more round trips.
const MaxChunkBytes = 32 << 10

// IsResult reports whether a kind is a reply (so a dispatcher ignores its own
// echoes and a caller recognises answers).
func IsResult(kind string) bool {
	return len(kind) > len(resultSuffix) && kind[len(kind)-len(resultSuffix):] == resultSuffix
}

// Command is the envelope every request payload rides in: a correlation id the
// reply echoes, plus the opaque per-op payload.
type Command struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Result is the envelope every reply rides in. Err is set when the operation
// failed; Payload carries the per-op success payload otherwise.
type Result struct {
	ID  string `json:"id"`
	Err string `json:"err,omitempty"`
	// Code classifies Err when the guest can say more than a message, so a
	// host branches on it rather than on text. Empty for an ordinary failure.
	Code    string          `json:"code,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// CodeUnsupported marks an op this guest's OS cannot perform. It is an answer,
// not a fault: the module is healthy, and a host should feature-gate rather
// than retry.
const CodeUnsupported = "unsupported"

// UnsupportedError is the error a handler returns for an op its OS cannot do.
// EncodeResult turns it into a Result carrying CodeUnsupported.
type UnsupportedError struct {
	Kind   string
	Reason string
}

func (e *UnsupportedError) Error() string {
	if e.Reason == "" {
		return e.Kind + " is not supported on this guest"
	}
	return e.Kind + " is not supported on this guest: " + e.Reason
}

// EncodeCommand marshals a per-op payload into a Command envelope's Data bytes.
func EncodeCommand(id string, payload any) ([]byte, error) {
	raw, err := marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	return marshal(Command{ID: id, Payload: raw})
}

// EncodeResult marshals a per-op payload (or an error) into a Result envelope's
// Data bytes.
func EncodeResult(id string, payload any, opErr error) ([]byte, error) {
	res := Result{ID: id}
	if opErr != nil {
		res.Err = opErr.Error()
		if _, ok := errors.AsType[*UnsupportedError](opErr); ok {
			res.Code = CodeUnsupported
		}
	} else {
		raw, err := marshalPayload(payload)
		if err != nil {
			return nil, err
		}
		res.Payload = raw
	}
	return marshal(res)
}

// EncodeEvent marshals an unsolicited event payload. Events carry no
// correlation envelope — there is no command to correlate them to — so the
// payload is the whole body.
func EncodeEvent(payload any) ([]byte, error) {
	if payload == nil {
		return nil, nil
	}
	return marshal(payload)
}

// EncodePayload marshals a per-op response payload, the bytes a handler
// returns for the dispatcher to put in its Result.
func EncodePayload(payload any) ([]byte, error) { return marshal(payload) }

func marshalPayload(payload any) (json.RawMessage, error) {
	if payload == nil {
		return nil, nil
	}
	return marshal(payload)
}

func marshal(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("weavewire: encoding: %w", err)
	}
	return data, nil
}

// --- Streams ---

// Chunk is one piece of a byte stream. Exec stdio and file transfer both use
// it, so there is one reassembly rule to get right rather than two.
//
// Ordering is already guaranteed — the hypervisor channel is a single owned
// pipe, so chunks arrive in the order they were sent. Seq is therefore not for
// re-ordering: it exists so a receiver can tell a GAP from a clean stream and
// fail loudly, because the failure this prevents (a file that transfers
// "successfully" with a hole in it) is silent otherwise.
//
// A stream ends exactly one way: a chunk with EOF set, carrying Err if the
// producer failed partway. A stream that simply stops is a broken channel, and
// the receiver must treat it as an error rather than as completion.
type Chunk struct {
	// StreamID scopes the chunk — an exec id for stdio, a transfer id for
	// files. Several streams share the channel concurrently.
	StreamID string `json:"stream_id"`
	// Seq counts from 0 within a stream, EOF chunk included.
	Seq uint64 `json:"seq"`
	// Data is at most MaxChunkBytes. Empty is legal (a bare EOF).
	Data []byte `json:"data,omitempty"`
	// EOF marks the final chunk of the stream.
	EOF bool `json:"eof,omitempty"`
	// Err explains why a stream ended early. Only meaningful with EOF.
	Err string `json:"err,omitempty"`
}

// --- Per-op payloads ---

// HelloResponse is the presence/hello reply: enough for the host to tell which
// agent build and OS answered, and redeploy if the build is stale.
type HelloResponse struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kernel   string `json:"kernel,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// PowerRequest is the shutdown/restart request payload. Reason is an optional
// audit string surfaced to the guest OS's shutdown facility where supported.
type PowerRequest struct {
	Reason string `json:"reason,omitempty"`
}

// PowerResponse acknowledges a power request was accepted. The host learns the
// action actually happened by the guest going away, not by this reply.
type PowerResponse struct {
	Accepted bool `json:"accepted"`
	// Command describes what the guest is about to do, for the diagnosis that
	// otherwise has nowhere to happen: a shutdown that is accepted and then
	// does nothing leaves no evidence inside a guest nobody can log into.
	Command string `json:"command,omitempty"`
}

// InventoryResponse is what the guest knows about itself. Fields it cannot read
// stay empty — a partial inventory is data, not an error, and the host would
// rather have eight facts than a failure because the ninth needed a privilege.
type InventoryResponse struct {
	CollectedAt time.Time `json:"collected_at"`
	Hostname    string    `json:"hostname"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	OSVersion   string    `json:"os_version,omitempty"`
	OSBuild     string    `json:"os_build,omitempty"`

	HardwareModel string `json:"hardware_model,omitempty"`
	SerialNumber  string `json:"serial_number,omitempty"`
	MemoryBytes   uint64 `json:"memory_bytes,omitempty"`
	UptimeSeconds uint64 `json:"uptime_seconds,omitempty"`
	CPUModel      string `json:"cpu_model,omitempty"`
	CPUCores      int    `json:"cpu_cores,omitempty"`

	// Interfaces is the reason this op exists at all. A hypervisor knows the
	// MAC it handed a guest, but not the address the guest ended up with —
	// DHCP, a static config, or a second NIC all happen inside. Asking the
	// guest is the only reliable answer.
	Interfaces []Interface `json:"interfaces,omitempty"`
}

// Interface is one network interface as the guest sees it.
type Interface struct {
	Name string `json:"name"`
	MAC  string `json:"mac,omitempty"`
	// Addrs are CIDR-form addresses, e.g. "192.168.64.7/24".
	Addrs []string `json:"addrs,omitempty"`
	Up    bool     `json:"up"`
	// Loopback is reported rather than filtered: the host asked what the guest
	// has, and a caller wanting a routable address can skip these itself.
	Loopback bool `json:"loopback,omitempty"`
	MTU      int  `json:"mtu,omitempty"`
}
