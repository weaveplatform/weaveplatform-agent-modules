package modulesdk

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Protocol is the protocol integer modules built with this SDK release
// speak. It moves only when the wire contract does (see agent-core's
// docs/PROTOCOL.md).
const Protocol uint32 = 1

// Capability names one probed fact about the host, e.g. "platform.osinfo".
// The vocabulary is owned by the protocol, not by modules.
type Capability string

// HealthStatus is a module's self-reported condition.
type HealthStatus int

const (
	HealthUnknown HealthStatus = iota
	HealthHealthy
	// HealthDegraded is first-class: the module is up but a backend it
	// needs is temporarily absent. The supervisor restarts Unhealthy
	// modules, not Degraded ones.
	HealthDegraded
	HealthUnhealthy
)

// Health is what Module.Health returns.
type Health struct {
	Status  HealthStatus
	Reason  string
	Details map[string]string
}

// Module is what every product's device-side half implements. See spec §5.
type Module interface {
	ID() string
	Requires() []Capability
	Init(context.Context, Host) error
	Start(context.Context) error
	Stop(context.Context) error
	Health() Health
}

// ConfigReceiver is optionally implemented by modules that take
// configuration. The runtime calls SetConfig with the core-delivered
// document (possibly empty) before Init; a returned error fails Init.
type ConfigReceiver interface {
	SetConfig(doc []byte) error
}

// Host is what core offers a module. It is closed by default: growing this
// interface is an architecture decision, not a pull request. Note what is
// deliberately absent: Schedule (in-process timer sugar — see Scheduled)
// and Platform (OS bindings can't cross a wire; link the platform seam
// directly).
type Host interface {
	Identity() Identity
	Transport() Transport
	Policy() PolicyReader
	// Store returns the module's namespaced store. ns partitions within
	// the module's own namespace ("" for the root); a module can never
	// reach another module's namespace, whatever it passes.
	Store(ns string) Store
	Events() Events
	UI() UIBroker
	Log() *slog.Logger
	// Registry is core's read-only list of the modules installed beside
	// this one, for a module that wants to know whether a peer is there
	// (and running) before it sends to it.
	Registry() Registry
}

// Scheduled is optionally implemented by a module that wants recurring
// work. The runtime collects Jobs after Init, runs each once at Start and
// then on its interval, and cancels them at Stop. Scheduling is in-process
// SDK sugar — nothing crosses to core — so it is a module-side interface,
// not a Host method.
type Scheduled interface {
	Jobs() []Job
}

// Identity answers who this device is. Modules never see private keys.
type Identity interface {
	WhoAmI(ctx context.Context) (DeviceIdentity, error)
	// Credential mints a token scoped to this module.
	Credential(ctx context.Context, scopes []string) (Credential, error)
}

// DeviceIdentity mirrors the platform's view of the device.
type DeviceIdentity struct {
	DeviceID  string
	Ephemeral bool
	Tenant    string
}

// Credential is a module-scoped bearer token.
type Credential struct {
	Token     string
	ExpiresAt time.Time
	Scopes    []string
}

// Peer names which channel a transport message travels on.
type Peer int

// PeerHypervisor is the host channel: the link to whatever drives this
// machine from directly outside it (a hypervisor, a container runtime). It is
// the only peer. The value matches the wire enum, where 1 is reserved.
const PeerHypervisor Peer = 2

// Message is one platform message, sent or received via core's transport.
type Message struct {
	Peer Peer
	Kind string
	Data []byte
}

// Transport sends and receives through core's authenticated channels.
// Modules never open their own sockets.
type Transport interface {
	// Send hands msg to core. With queueOffline, an unreachable peer
	// queues the message instead of failing; delivered reports which
	// happened.
	Send(ctx context.Context, msg Message, queueOffline bool) (delivered bool, err error)
	// Receive delivers inbound messages addressed to this module until
	// ctx ends. The channel closes on ctx cancellation or stream failure.
	Receive(ctx context.Context) (<-chan Message, error)
}

// PolicyDocument is the module-scoped policy payload as delivered by core.
type PolicyDocument struct {
	Revision uint64
	Data     []byte
	// SchemaVersion and ContentType are module-owned envelope metadata so a
	// module can evolve its policy shape safely (core never interprets the
	// payload). Zero/empty when the server didn't set them.
	SchemaVersion uint32
	ContentType   string
}

// PolicyReader reads and watches this module's policy, which core takes from
// the device's policy set. Policy is authoritative over the module's config.
type PolicyReader interface {
	Get(ctx context.Context) (PolicyDocument, error)
	// Watch yields the current document immediately, then again on every
	// change. The channel closes on ctx cancellation or stream failure.
	Watch(ctx context.Context) (<-chan PolicyDocument, error)
}

// Store is the module's encrypted, namespaced key/value store.
type Store interface {
	Get(ctx context.Context, key string) (value []byte, found bool, err error)
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Event is one bus event. Topic arrives prefixed with the publisher's
// module id, stamped by core — subscribers can trust the origin.
type Event struct {
	Topic       string
	Data        []byte
	PublishedAt time.Time
	// Sequence is a monotonic per-topic counter. A gap versus the last
	// Sequence a subscriber saw for a topic means it missed events
	// (delivery is at-most-once) and should re-pull state.
	Sequence uint64
}

// Events is the inter-module bus. Modules do not import each other; this
// is the only lateral channel.
type Events interface {
	// Publish emits on topic; core prefixes it with this module's id.
	Publish(ctx context.Context, topic string, data []byte) error
	// Subscribe delivers matching events until ctx ends. Patterns are
	// exact topics or prefix globs like "example.*".
	Subscribe(ctx context.Context, topics ...string) (<-chan Event, error)
}

// Surface is UI declared as data. Modules never draw; the portal renders.
type Surface struct {
	ID    string
	Title string
	Kind  string
	Data  []byte
}

// UIBroker collects the module's declared surfaces. At protocol 1,
// surfaces are declared during Init and carried in the Init response;
// declaring later is not yet supported.
type UIBroker interface {
	Declare(surfaces ...Surface) error
}

// ErrSurfacesAfterInit is what Declare returns once Init has completed.
var ErrSurfacesAfterInit = errors.New(
	"modulesdk: declaring surfaces after Init is not supported at protocol 1",
)

// Job is a recurring unit of work. Run is invoked once immediately after
// Start, then on each interval, until the module stops. Runs do not
// overlap.
//
// The interval is re-read every cycle: set EveryFunc for an interval that
// can change at runtime (e.g. driven by policy), or Every for a fixed one.
// EveryFunc takes precedence when both are set; a non-positive interval
// ends the job.
type Job struct {
	Name      string
	Every     time.Duration
	EveryFunc func() time.Duration
	Run       func(context.Context)
}
