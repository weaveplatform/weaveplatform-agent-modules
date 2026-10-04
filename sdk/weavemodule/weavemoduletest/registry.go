package weavemoduletest

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
)

// Registry is an in-memory modulesdk.Registry, for a module test that sets
// which other modules are installed. Every change moves the revision and
// wakes watchers, as core's does.
type Registry struct {
	mu       sync.Mutex
	revision uint64
	modules  map[string]modulesdk.RegisteredModule
	err      error
	watchers []chan struct{}
}

// NewRegistry returns an empty registry at revision 0.
func NewRegistry() *Registry {
	return &Registry{modules: make(map[string]modulesdk.RegisteredModule)}
}

// Set replaces the whole registry with mods.
func (r *Registry) Set(mods ...modulesdk.RegisteredModule) {
	r.change(func() {
		clear(r.modules)
		for _, m := range mods {
			r.modules[m.ID] = m
		}
	})
}

// SetModule adds m, or replaces the module with its ID.
func (r *Registry) SetModule(m modulesdk.RegisteredModule) {
	r.change(func() { r.modules[m.ID] = m })
}

// Remove drops the module with id.
func (r *Registry) Remove(id string) {
	r.change(func() { delete(r.modules, id) })
}

// Fail makes List and Watch return err — modulesdk.ErrRegistryUnsupported
// for a core from before the registry. Nil restores them.
func (r *Registry) Fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *Registry) change(f func()) {
	r.mu.Lock()
	f()
	r.revision++
	watchers := r.watchers
	r.mu.Unlock()
	for _, w := range watchers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// List implements modulesdk.Registry.
func (r *Registry) List(context.Context) (modulesdk.RegistrySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return modulesdk.RegistrySnapshot{}, r.err
	}
	return r.snapshotLocked(), nil
}

// Watch implements modulesdk.Registry: the registry now, then after each
// change, coalesced, until ctx ends.
func (r *Registry) Watch(ctx context.Context) (<-chan modulesdk.RegistrySnapshot, error) {
	wake := make(chan struct{}, 1)
	r.mu.Lock()
	if r.err != nil {
		err := r.err
		r.mu.Unlock()
		return nil, err
	}
	r.watchers = append(r.watchers, wake)
	r.mu.Unlock()

	out := make(chan modulesdk.RegistrySnapshot)
	go func() {
		defer close(out)
		defer r.unwatch(wake)
		for {
			r.mu.Lock()
			snap := r.snapshotLocked()
			r.mu.Unlock()
			select {
			case out <- snap:
			case <-ctx.Done():
				return
			}
			select {
			case <-wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (r *Registry) unwatch(wake chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watchers = slices.DeleteFunc(r.watchers, func(w chan struct{}) bool { return w == wake })
}

func (r *Registry) snapshotLocked() modulesdk.RegistrySnapshot {
	out := modulesdk.RegistrySnapshot{
		Revision: r.revision,
		Modules:  make([]modulesdk.RegisteredModule, 0, len(r.modules)),
	}
	for _, m := range r.modules {
		out.Modules = append(out.Modules, m)
	}
	slices.SortFunc(out.Modules, func(a, b modulesdk.RegisteredModule) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

var _ modulesdk.Registry = (*Registry)(nil)
