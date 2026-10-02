// Package guestmodule is the runtime every guestweave capability module is
// built on. It implements modulesdk.Module once: it wires a guestagent
// Dispatcher to core's hypervisor transport, lets one capability's Service
// register a handler per op, and runs the receive loop.
//
// A per-OS module (modules/guestweave-<os>-<capability>) is then a main.go and
// an OS backend:
//
//	func main() {
//		modulesdk.Serve(guestmodule.New(
//			guestmodule.ModuleID("linux", guestwire.Exec),
//			guestexec.NewService(guestexec.UnixStarter{}),
//		))
//	}
//
// Everything OS-neutral — decoding, policy, streaming, reply ordering — lives
// in the capability's pkg/guest<capability> Service, so the three OS variants
// differ only in the backend they hand it.
package guestmodule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestagent"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// Service is one capability's guest-side logic.
type Service interface {
	// Capability names what this service serves. Every kind it registers
	// must belong to it.
	Capability() guestwire.Capability
	// Register installs a handler per op. It runs during Init, so it may read
	// the Host (policy, events) but must not start long-lived work.
	Register(r *Registrar) error
}

// Stopper is implemented by a Service that owns work to tear down when the
// module stops (running processes, open transfers).
type Stopper interface {
	Stop(ctx context.Context) error
}

// HealthReporter is implemented by a Service whose health can be other than
// healthy, for example while an OS facility it needs is absent.
type HealthReporter interface {
	Health() modulesdk.Health
}

// ModuleID is the manifest id for a capability's module on one guest OS:
// guestweave-<os>-<capability>. os is linux, macos or windows.
func ModuleID(os string, c guestwire.Capability) string {
	return guestwire.Namespace + "-" + os + "-" + string(c)
}

// Module implements modulesdk.Module for one capability on one OS.
type Module struct {
	id  string
	svc Service

	log  *slog.Logger
	host modulesdk.Host
	disp *guestagent.Dispatcher

	mu     sync.Mutex
	kinds  []string
	cancel context.CancelFunc
}

// New builds a module with manifest id id serving svc.
func New(id string, svc Service) *Module { return &Module{id: id, svc: svc} }

// ID implements modulesdk.Module.
func (m *Module) ID() string { return m.id }

// Address is the channel address the module answers to. Its manifest must
// declare the same value, or core routes the host's commands elsewhere.
func (m *Module) Address() string { return m.svc.Capability().Address() }

// Requires implements modulesdk.Module. A guestweave module only makes sense
// inside a guest with a hypervisor channel, so core never launches one on a
// host without it.
func (m *Module) Requires() []modulesdk.Capability {
	return []modulesdk.Capability{"hypervisor.channel"}
}

// Init wires the dispatcher and lets the service register its handlers.
//
// A service that registers a kind outside its capability fails Init rather
// than serving it: core routes by address, so that handler could never be
// reached, and the mistake would otherwise surface only as a guest that never
// answers.
func (m *Module) Init(_ context.Context, host modulesdk.Host) error {
	m.host = host
	m.log = host.Log()
	if m.log == nil {
		m.log = slog.Default()
	}
	m.disp = guestagent.New(host.Transport(), m.log)
	r := &Registrar{capability: m.svc.Capability(), disp: m.disp, host: host, log: m.log}
	if err := m.svc.Register(r); err != nil {
		return fmt.Errorf("guestmodule: %s: %w", m.id, err)
	}
	if err := r.err(); err != nil {
		return fmt.Errorf("guestmodule: %s: %w", m.id, err)
	}
	m.mu.Lock()
	m.kinds = m.disp.Kinds()
	m.mu.Unlock()
	return nil
}

// Kinds reports the command kinds this module serves after Init, sorted.
func (m *Module) Kinds() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.kinds)
}

// Start begins consuming inbound hypervisor commands. The receive loop runs
// on a background context so it outlives the Start RPC; Stop cancels it.
func (m *Module) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	in, err := m.host.Transport().Receive(runCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("guestmodule: %s: receiving: %w", m.id, err)
	}
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	go m.disp.Run(runCtx, in)
	return nil
}

// Stop cancels the receive loop, then lets the service tear down its own work.
func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s, ok := m.svc.(Stopper); ok {
		if err := s.Stop(ctx); err != nil {
			return fmt.Errorf("guestmodule: %s: stopping: %w", m.id, err)
		}
	}
	return nil
}

// Health implements modulesdk.Module: the service's own report when it has
// one, healthy otherwise. A lost hypervisor channel surfaces as core's
// transport error, not here.
func (m *Module) Health() modulesdk.Health {
	if h, ok := m.svc.(HealthReporter); ok {
		return h.Health()
	}
	return modulesdk.Health{Status: modulesdk.HealthHealthy}
}

// Registrar is what a Service registers its handlers through.
type Registrar struct {
	capability guestwire.Capability
	disp       *guestagent.Dispatcher
	host       modulesdk.Host
	log        *slog.Logger
	errs       []error
}

// Handle registers h for kind.
func (r *Registrar) Handle(kind string, h guestagent.Handler) {
	if r.check(kind) {
		r.disp.Handle(kind, h)
	}
}

// HandleDeferred registers a handler whose work runs after its reply is on the
// wire. See guestagent.DeferredHandler.
func (r *Registrar) HandleDeferred(kind string, h guestagent.DeferredHandler) {
	if r.check(kind) {
		r.disp.HandleDeferred(kind, h)
	}
}

// Unsupported registers kind as an op this OS cannot perform. The host gets a
// guestwire.CodeUnsupported result naming reason, which it can feature-gate
// on, instead of an "unknown command kind" that reads like a broken module.
func (r *Registrar) Unsupported(kind, reason string) {
	r.Handle(kind, func(context.Context, []byte) ([]byte, error) {
		return nil, &guestwire.UnsupportedError{Kind: kind, Reason: reason}
	})
}

// Emitter sends unsolicited events (stream chunks, exits) on the module's
// transport.
func (r *Registrar) Emitter() guestagent.Emitter { return r.disp.Emitter() }

// Host is core's host surface, for services that read policy or publish
// events.
func (r *Registrar) Host() modulesdk.Host { return r.host }

// Log is the module's logger.
func (r *Registrar) Log() *slog.Logger { return r.log }

// check records why kind may not be registered, reporting whether it may.
func (r *Registrar) check(kind string) bool {
	c, ok := guestwire.CapabilityOf(kind)
	switch {
	case guestwire.IsResult(kind):
		r.errs = append(r.errs, fmt.Errorf("%w: %q is a reply kind", errBadKind, kind))
	case !ok || c != r.capability:
		r.errs = append(r.errs, fmt.Errorf("%w: %q is not a %s op", errBadKind, kind, r.capability))
	case slices.Contains(r.disp.Kinds(), kind):
		r.errs = append(r.errs, fmt.Errorf("%w: %q is registered twice", errBadKind, kind))
	default:
		return true
	}
	return false
}

func (r *Registrar) err() error { return errors.Join(r.errs...) }

var errBadKind = errors.New("cannot register")

// ServedKinds reports the kinds svc registers, sorted, without a running
// core: Register runs against a host whose surfaces are all absent.
func ServedKinds(svc Service) ([]string, error) {
	m := New("parity", svc)
	if err := m.Init(context.Background(), absentHost{}); err != nil {
		return nil, err
	}
	return m.Kinds(), nil
}

// CheckParity reports an error unless svc serves exactly its capability's
// ops. Each per-OS module runs it against its real backend; it is what turns
// "forgot Windows" into a failing test rather than a command that goes
// unanswered in one guest.
func CheckParity(svc Service) error {
	got, err := ServedKinds(svc)
	if err != nil {
		return err
	}
	if want := svc.Capability().Ops(); !slices.Equal(got, want) {
		return fmt.Errorf(
			"%w: %s serves %v, the contract is %v",
			errParity,
			svc.Capability(),
			got,
			want,
		)
	}
	return nil
}

var errParity = errors.New("guestmodule: capability parity")

// absentHost is a Host with nothing behind it, for registration dry runs.
type absentHost struct{}

func (absentHost) Identity() modulesdk.Identity   { return nil }
func (absentHost) Transport() modulesdk.Transport { return nil }
func (absentHost) Policy() modulesdk.PolicyReader { return nil }
func (absentHost) Store(string) modulesdk.Store   { return nil }
func (absentHost) Events() modulesdk.Events       { return nil }
func (absentHost) UI() modulesdk.UIBroker         { return nil }
func (absentHost) Log() *slog.Logger              { return slog.New(slog.DiscardHandler) }
