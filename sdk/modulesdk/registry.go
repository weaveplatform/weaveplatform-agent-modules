package modulesdk

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
)

// Registry is core's read-only view of the modules installed beside this one
// (RegistryService, weave-agent v0.9.2 and later). It is how a module tells a
// peer that is missing from one that is merely not running yet before it
// sends to it, rather than by waiting for an answer that never comes. It
// shows only what a module may act on — identity, address, state and health.
type Registry interface {
	// List returns the registry now.
	List(ctx context.Context) (RegistrySnapshot, error)
	// Watch yields the registry immediately, then again after every change:
	// a module added or removed, or a module's state or health changing.
	// Snapshots coalesce, so a slow reader skips to the latest. The channel
	// closes on ctx cancellation or stream failure.
	Watch(ctx context.Context) (<-chan RegistrySnapshot, error)
}

// ErrRegistryUnsupported matches (via errors.Is) a core that has no
// RegistryService: weave-agent before v0.9.2. Fall back to asking the peer
// directly and treating silence as absence.
var ErrRegistryUnsupported = errors.New(
	"modulesdk: core has no module registry (weave-agent v0.9.2 or later serves it)",
)

// RegistrySnapshot is the registry at one revision.
type RegistrySnapshot struct {
	// Revision increases with every change for the life of the core
	// process; of two snapshots, keep the higher. It restarts when core
	// does.
	Revision uint64
	// Modules is sorted by ID.
	Modules []RegisteredModule
}

// RegisteredModule is one installed module.
type RegisteredModule struct {
	ID      string
	Version string
	// Address is the name the module answers to on the host channel: a
	// capability's address, such as "weave.clipboard", or its ID.
	Address string
	// State is the lifecycle state: ModuleStateRunning and the others
	// below. Treat one not listed as not running; the vocabulary may grow.
	State string
	// Health is the module's last health report; HealthUnknown until core
	// has polled it.
	Health Health
}

// Module lifecycle states, as RegisteredModule.State reports them.
const (
	ModuleStatePending             = "pending"
	ModuleStateStarting            = "starting"
	ModuleStateRunning             = "running"
	ModuleStateBackoff             = "backoff"
	ModuleStateStartLimited        = "start-limited"
	ModuleStateUnsupportedProtocol = "unsupported-protocol"
	ModuleStateRequirementsUnmet   = "requirements-unmet"
	ModuleStateWaitingForSession   = "waiting-for-session"
	ModuleStateStopped             = "stopped"
)

// Running reports whether the module is up.
func (m RegisteredModule) Running() bool { return m.State == ModuleStateRunning }

// Module finds the module answering to address (or, failing that, with that
// ID).
func (s RegistrySnapshot) Module(address string) (RegisteredModule, bool) {
	for _, m := range s.Modules {
		if m.Address == address {
			return m, true
		}
	}
	for _, m := range s.Modules {
		if m.ID == address {
			return m, true
		}
	}
	return RegisteredModule{}, false
}

// Installed reports whether a module answers to address, running or not.
func (s RegistrySnapshot) Installed(address string) bool {
	_, ok := s.Module(address)
	return ok
}

// --- the host client's Registry ---

func (h *hostClient) Registry() Registry { return registryClient{h} }

type registryClient struct{ h *hostClient }

func (c registryClient) List(ctx context.Context) (RegistrySnapshot, error) {
	snap, err := c.h.registry.List(ctx, &agentv1.RegistryListRequest{})
	if err != nil {
		return RegistrySnapshot{}, registryErr(err)
	}
	return registrySnapshot(snap), nil
}

// Watch reads the first snapshot before returning, so a core without the
// service fails here with ErrRegistryUnsupported rather than as a channel
// that closes at once.
func (c registryClient) Watch(ctx context.Context) (<-chan RegistrySnapshot, error) {
	stream, err := c.h.registry.Watch(ctx, &agentv1.RegistryWatchRequest{})
	if err != nil {
		return nil, registryErr(err)
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, registryErr(err)
	}
	out := make(chan RegistrySnapshot)
	go func() {
		defer close(out)
		for snap := first; ; {
			select {
			case out <- registrySnapshot(snap):
			case <-ctx.Done():
				return
			}
			if snap, err = stream.Recv(); err != nil {
				return
			}
		}
	}()
	return out, nil
}

// registryErr maps a core that does not serve RegistryService to
// ErrRegistryUnsupported and anything else as the other host services do.
func registryErr(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("%w: %w", ErrRegistryUnsupported, err)
	}
	return sentinelErr(err)
}

func registrySnapshot(s *agentv1.RegistrySnapshot) RegistrySnapshot {
	out := RegistrySnapshot{
		Revision: s.GetRevision(),
		Modules:  make([]RegisteredModule, 0, len(s.GetModules())),
	}
	for _, m := range s.GetModules() {
		out.Modules = append(out.Modules, RegisteredModule{
			ID:      m.GetId(),
			Version: m.GetVersion(),
			Address: m.GetAddress(),
			State:   m.GetState(),
			Health: Health{
				Status:  healthStatus(m.GetHealth().GetStatus()),
				Reason:  m.GetHealth().GetReason(),
				Details: m.GetHealth().GetDetails(),
			},
		})
	}
	return out
}

// healthStatus is the wire enum as a HealthStatus; a status this SDK does
// not know reads as unknown.
func healthStatus(s agentv1.Health_Status) HealthStatus {
	switch s {
	case agentv1.Health_STATUS_HEALTHY:
		return HealthHealthy
	case agentv1.Health_STATUS_DEGRADED:
		return HealthDegraded
	case agentv1.Health_STATUS_UNHEALTHY:
		return HealthUnhealthy
	default:
		return HealthUnknown
	}
}
