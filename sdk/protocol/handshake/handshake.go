// Package handshake is the shared vocabulary of the core↔module handshake:
// the environment core sets, the one stdout line the module answers with,
// and the exit code that means "protocol out of window". Both sides share
// this vocabulary; neither side imports the other. Core keeps its own copy
// (weaveplatform-agent-core's internal/protocol/handshake), so a change here is
// a protocol change and must land in core as well.
//
// The flow (agent-core's docs/PROTOCOL.md owns the normative text):
//
//	core → module    env: EnvProtocolMin, EnvProtocolMax, EnvToken,
//	                      EnvHostAddr, EnvSocketDir
//	module → core    stdout: WEAVE|1|<protocol>|<network>|<addr>
//	core → module    gRPC ModuleService.Init on <addr>
//	module → core    gRPC dial EnvHostAddr presenting the token
package handshake

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/ipc"
)

// Environment variables core sets before exec'ing a module.
const (
	// EnvProtocolMin and EnvProtocolMax advertise core's accepted window.
	EnvProtocolMin = "WEAVE_PROTOCOL_MIN"
	EnvProtocolMax = "WEAVE_PROTOCOL_MAX"
	// EnvToken is the one-time token the module presents when dialing the
	// host address; core binds the connection to the module's identity.
	EnvToken = "WEAVE_HANDSHAKE_TOKEN"
	// EnvHostAddr is where core serves this module's host services.
	EnvHostAddr = "WEAVE_HOST_ADDR"
	// EnvSocketDir is the core-owned directory the module's own socket
	// must be created in (ignored for Windows named pipes).
	EnvSocketDir = "WEAVE_SOCKET_DIR"
	// Config is NOT an env var: it rides InitRequest.Config on the wire.
)

// winpipeNetwork is a second spelling of ipc.NetworkPipe that protocol-1
// modules may print. Core runs every protocol-1 module, so Parse accepts it
// and normalises it; nothing here writes it.
const winpipeNetwork = "winpipe"

// ErrInvalidLine reports a handshake line that does not parse: wrong shape,
// unsupported line version, bad protocol, unknown network or no address.
var ErrInvalidLine = errors.New("handshake: invalid line")

// ErrInvalidWindow reports protocol window environment values that are not
// numbers, or that describe an empty or zero-based window.
var ErrInvalidWindow = errors.New("handshake: invalid protocol window")

// LineVersion is the handshake format version — the leading field of the
// stdout line, distinct from the protocol integer that follows it.
const LineVersion = 1

// ExitProtocolUnsupported (EX_CONFIG) is the exit code a module uses when
// its protocol falls outside the advertised window. Core records
// "protocol unsupported" and does not restart — refusal is clean, never a
// crash loop.
const ExitProtocolUnsupported = 78

// TokenMetadataKey is the gRPC metadata key the module presents the
// one-time token under when dialing the host address.
const TokenMetadataKey = "weave-handshake-token"

// Line is the module's one-line stdout answer.
type Line struct {
	// Protocol the module speaks (already validated against the window).
	Protocol uint32
	// Network is "unix" or "npipe".
	Network string
	// Addr is the socket path or pipe name the module is listening on.
	Addr string
}

// Format renders the line without a trailing newline.
func (l Line) Format() string {
	return fmt.Sprintf("WEAVE|%d|%d|%s|%s", LineVersion, l.Protocol, l.Network, l.Addr)
}

// Parse parses a module's stdout line.
func Parse(s string) (Line, error) {
	parts := strings.Split(strings.TrimSpace(s), "|")
	if len(parts) != 5 || parts[0] != "WEAVE" {
		return Line{}, fmt.Errorf("%w: malformed %q", ErrInvalidLine, s)
	}
	v, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil || v != LineVersion {
		return Line{}, fmt.Errorf("%w: unsupported line version %q", ErrInvalidLine, parts[1])
	}
	p, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil || p == 0 {
		return Line{}, fmt.Errorf("%w: bad protocol %q", ErrInvalidLine, parts[2])
	}
	network := parts[3]
	// "winpipe" is what this field was called before the rename to "npipe", and
	// it is still what a module built against an SDK older than v0.3 prints.
	// Refusing it broke the protocol-1 promise on Windows only — the unix token
	// never changed, so every unix guest kept working and nothing showed it.
	// Accept the old spelling and normalise; the alias costs one comparison and
	// buys compatibility with every module already built.
	if network == winpipeNetwork {
		network = ipc.NetworkPipe
	}
	if network != ipc.NetworkUnix && network != ipc.NetworkPipe {
		return Line{}, fmt.Errorf("%w: unknown network %q", ErrInvalidLine, parts[3])
	}
	if parts[4] == "" {
		return Line{}, fmt.Errorf("%w: empty address", ErrInvalidLine)
	}
	return Line{Protocol: uint32(p), Network: network, Addr: parts[4]}, nil
}

// Window is core's advertised protocol range as read from (or written to)
// the environment.
type Window struct {
	Min, Max uint32
}

// Contains reports whether p falls inside the window.
func (w Window) Contains(p uint32) bool { return p >= w.Min && p <= w.Max }

// ParseWindow reads the two env values (as strings) into a Window.
func ParseWindow(min, max string) (Window, error) {
	lo, err := strconv.ParseUint(min, 10, 32)
	if err != nil {
		return Window{}, fmt.Errorf("%w: %s %q: %w", ErrInvalidWindow, EnvProtocolMin, min, err)
	}
	hi, err := strconv.ParseUint(max, 10, 32)
	if err != nil {
		return Window{}, fmt.Errorf("%w: %s %q: %w", ErrInvalidWindow, EnvProtocolMax, max, err)
	}
	if lo == 0 || hi < lo {
		return Window{}, fmt.Errorf("%w: [%d,%d]", ErrInvalidWindow, lo, hi)
	}
	return Window{Min: uint32(lo), Max: uint32(hi)}, nil
}
