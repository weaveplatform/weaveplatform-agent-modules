package weaveclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
)

// The module registry and undeliverable-message replies (weave-agent v0.9.2
// and later). Core keeps a table of the modules installed in the guest and
// shares it with an authenticated host: on request (modules.list) and
// whenever it changes (modules.changed). And when core cannot hand a frame to
// a module — none is installed at that address, it is not running, or its
// queue is full — it says so (delivery.failed) instead of leaving the host to
// wait out a timeout. Both are channel-level frames addressed to hvchannel,
// correlated by the envelope id, which every Call sets to its command id.

// ModulesSnapshot is the guest's whole module registry at one revision.
// Modules is sorted by ID. Treat it as read-only: snapshots handed to
// different callers may share their per-module slices.
type ModulesSnapshot = hvchannel.ModulesSnapshot

// ModuleInfo is one installed module as core reports it. Address is what a
// call is sent to — a capability's address, such as "weave.clipboard".
type ModuleInfo = hvchannel.ModuleInfo

// ModuleHealth is a module's last health report.
type ModuleHealth = hvchannel.ModuleHealth

// ModuleStateRunning is the ModuleInfo.State of a module that is up. Every
// other state — pending, starting, backoff, start-limited,
// unsupported-protocol, requirements-unmet, waiting-for-session, stopped, or
// one added later — means it will not answer now.
const ModuleStateRunning = "running"

// ModuleStateWaitingForSession is the state of a per-user-console module
// (clipboard, display) while nobody is logged in at the guest's console.
const ModuleStateWaitingForSession = "waiting-for-session"

// ErrRegistryUnsupported reports a core that did not answer modules.list
// within Options.RegistryTimeout, on a connection with no sign that core has
// a registry — or with positive evidence that it has none
// (Options.CoreVersion before v0.9.2). A weave-agent older than v0.9.2
// ignores the request rather than refusing it, so silence is all there is to
// go on; the error also matches context.DeadlineExceeded. Feature-gate the
// way a host did before the registry existed: call, and treat no answer as
// absent.
//
// It is a conclusion about this one call, not about the connection: once core
// shows it has a registry — a snapshot, a modules.changed push, a
// delivery.failed, a refusal that echoes its frame's id — a later timeout is
// ErrRegistryTimeout instead, and a late answer to the call that timed out
// still lands in Snapshot.
var ErrRegistryUnsupported = errors.New("weaveclient: core did not answer modules.list")

// ErrRegistryTimeout reports a core that did not answer modules.list within
// Options.RegistryTimeout although this connection has shown that it has a
// registry (see ErrRegistryUnsupported for what counts), or although
// Options.CoreVersion says it is v0.9.2 or later. Core answers modules.list
// from memory without asking any module, so the channel itself is not
// moving: core is stalled or wedged, or the transport is. Retrying the
// registry is pointless until it moves again; do not downgrade to treating
// the guest as an older core. The error also matches
// context.DeadlineExceeded, and never ErrRegistryUnsupported.
var ErrRegistryTimeout = errors.New(
	"weaveclient: core has a module registry but did not answer modules.list",
)

// ErrUndeliverable matches (via errors.Is) every DeliveryError, whatever its
// reason: core could not hand the frame to a module, so no module ever saw it.
var ErrUndeliverable = errors.New("weaveclient: core could not deliver the message")

// ErrModuleNotInstalled matches a call to an address no module in the guest
// answers to. Retrying will not help; feature-gate instead.
var ErrModuleNotInstalled = errors.New("weaveclient: no module installed for the address")

// ErrModuleNotRunning matches a call to a module that is installed but not
// running — starting, crashed, stopped, refused, or waiting for a console
// session (DeliveryError.State says which). It may answer later: wait for
// OnModulesChanged to report it running, then retry.
var ErrModuleNotRunning = errors.New("weaveclient: the module is not running")

// ErrModuleBusy matches a call core dropped because the module's receive
// queue was full. A retry with backoff may succeed.
var ErrModuleBusy = errors.New("weaveclient: the module is busy")

// DeliveryError is core's delivery.failed for one frame: why the module at
// Module never received Kind. It matches ErrUndeliverable and the sentinel
// for its Reason with errors.Is — and ErrNoSession too when the module is
// waiting for a console session, which is what that error always meant.
type DeliveryError struct {
	// Module and Kind are the undeliverable frame's address and kind.
	Module string
	Kind   string
	// Reason is hvchannel.ReasonNotInstalled, ReasonNotRunning or
	// ReasonBusy, or one added after this client was built.
	Reason string
	// State is the module's lifecycle state, for ReasonNotRunning.
	State string
	// Detail is core's explanation, when it has one.
	Detail string
}

func (e *DeliveryError) Error() string {
	msg := "weave: " + e.Kind + ": core could not deliver it to " + e.Module + ": " + e.Reason
	if e.State != "" {
		msg += " (" + e.State + ")"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Is matches the sentinels: ErrUndeliverable always, and the one for Reason.
func (e *DeliveryError) Is(target error) bool {
	switch target {
	case ErrUndeliverable:
		return true
	case ErrModuleNotInstalled:
		return e.Reason == hvchannel.ReasonNotInstalled
	case ErrModuleNotRunning:
		return e.Reason == hvchannel.ReasonNotRunning
	case ErrModuleBusy:
		return e.Reason == hvchannel.ReasonBusy
	case ErrNoSession:
		return e.Reason == hvchannel.ReasonNotRunning && e.State == ModuleStateWaitingForSession
	}
	return false
}

// Modules asks core for the guest's module registry and returns the newest
// snapshot this client has: the answer, or a modules.changed push that
// overtook it with a higher revision.
//
// The channel must be authenticated: core refuses the request otherwise, and
// Modules returns ErrNotAuthenticated. A core older than weave-agent v0.9.2
// ignores it, so the wait is bounded by Options.RegistryTimeout and ends in
// ErrRegistryUnsupported rather than hanging — or in ErrRegistryTimeout when
// core is known to have a registry, and the silence means the channel is
// stuck rather than the core old.
func (c *Client) Modules(ctx context.Context) (ModulesSnapshot, error) {
	return c.listModules(ctx, c.registryTimeout)
}

// listModules is Modules with its own bound; zero or negative waits as long
// as ctx allows.
func (c *Client) listModules(ctx context.Context, bound time.Duration) (ModulesSnapshot, error) {
	id := c.newID()
	reply := make(chan pendingReply, 1)
	c.mu.Lock()
	if c.closed {
		err := c.cause
		c.mu.Unlock()
		return ModulesSnapshot{}, err
	}
	c.pending[id] = reply
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	var unanswered <-chan time.Time
	if bound > 0 {
		timer := time.NewTimer(bound)
		defer timer.Stop()
		unanswered = timer.C
	}
	if err := c.sendControl(hvchannel.KindModulesList, id, nil); err != nil {
		return ModulesSnapshot{}, err
	}
	select {
	case r := <-reply:
		if r.err != nil {
			return ModulesSnapshot{}, r.err
		}
		return cloneSnapshot(*r.snap), nil
	case <-unanswered:
		return ModulesSnapshot{}, c.registryUnanswered(bound)
	case <-ctx.Done():
		return ModulesSnapshot{}, ctx.Err()
	case <-c.done:
		return ModulesSnapshot{}, c.Err()
	}
}

// registryUnanswered is the error for a modules.list that went unanswered,
// read from the evidence as it stands now rather than when the call began: a
// push that arrived during the wait already proves core has a registry.
func (c *Client) registryUnanswered(bound time.Duration) error {
	c.mu.Lock()
	evidence := c.coreRegistry
	c.mu.Unlock()
	switch evidence {
	case registryPresent:
		return fmt.Errorf(
			"%w in %s; the channel is not moving: %w",
			ErrRegistryTimeout, bound, context.DeadlineExceeded,
		)
	case registryAbsent:
		return fmt.Errorf(
			"%w in %s; core's version predates the registry (weave-agent v0.9.2): %w",
			ErrRegistryUnsupported, bound, context.DeadlineExceeded,
		)
	}
	return fmt.Errorf(
		"%w in %s; weave-agent before v0.9.2 has no module registry: %w",
		ErrRegistryUnsupported, bound, context.DeadlineExceeded,
	)
}

// registryEvidence is what a connection has shown about core's registry.
type registryEvidence int

const (
	// registryUnknown: nothing either way. A silent modules.list is read as
	// an older core, as it always was.
	registryUnknown registryEvidence = iota
	// registryPresent: core has answered or pushed a snapshot, sent a
	// delivery.failed or echoed an id on a refusal — all of which arrived
	// with the registry in v0.9.2 — or Options.CoreVersion says v0.9.2+.
	registryPresent
	// registryAbsent: Options.CoreVersion says core predates v0.9.2. Only
	// the caller can say that; silence on the wire never concludes it, so
	// anything the wire later shows overrides it.
	registryAbsent
)

// sawRegistry records that core has just done something only a core with
// the registry does. It overrides any earlier conclusion, including one from
// Options.CoreVersion: what core does on the wire is better evidence than
// what a caller believed it was running.
func (c *Client) sawRegistry() {
	c.mu.Lock()
	c.coreRegistry = registryPresent
	c.mu.Unlock()
}

// reportsUndeliverable reports whether core is known to answer a frame it
// cannot deliver with delivery.failed — which arrived with the registry, so
// the same evidence says both. When an authentication's registry fetch is
// still out, it waits for that first: an exec started straight after
// authenticating would otherwise send its first input before the answer that
// says how to send it.
func (c *Client) reportsUndeliverable(ctx context.Context) (bool, error) {
	c.mu.Lock()
	evidence, refreshing := c.coreRegistry, c.refreshing
	c.mu.Unlock()
	if evidence == registryUnknown && refreshing != nil {
		select {
		case <-refreshing:
		case <-ctx.Done():
			return false, ctx.Err()
		case <-c.done:
			return false, c.Err()
		}
		c.mu.Lock()
		evidence = c.coreRegistry
		c.mu.Unlock()
	}
	return evidence == registryPresent, nil
}

// registryVersion is the first weave-agent with the registry and
// delivery.failed.
var registryVersion = [3]int{0, 9, 2}

// evidenceFromVersion reads Options.CoreVersion: "v0.9.2" or "0.9.2". A
// pre-release or build suffix ("-rc.1", "+abc") is ignored and only the
// numbers compared: a version string is a hint, and the wire overrides it
// either way.
func evidenceFromVersion(v string) registryEvidence {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return registryUnknown
	}
	var got [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return registryUnknown
		}
		got[i] = n
	}
	if slices.Compare(got[:], registryVersion[:]) >= 0 {
		return registryPresent
	}
	return registryAbsent
}

// OnModulesChanged calls fn each time the client's snapshot of the registry
// advances: for every modules.changed core pushes, and for a Modules answer
// newer than what the client had, but never for one older — a push and an
// answer can arrive in either order, and only the higher revision is kept.
// Like On, fn runs on the read loop and must not block, and it stays
// registered for the life of the client.
//
// Core pushes only to an authenticated channel, and the client asks for a
// full snapshot each time Authenticate succeeds, so a handler registered
// before authenticating sees the registry from the start.
func (c *Client) OnModulesChanged(fn func(ModulesSnapshot)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.moduleHandlers = append(c.moduleHandlers, fn)
}

// Snapshot returns the newest registry snapshot the client holds without
// asking core, and false when it holds none: before authenticating, before
// the first answer arrives, or against a core with no registry.
func (c *Client) Snapshot() (ModulesSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.registry == nil {
		return ModulesSnapshot{}, false
	}
	return cloneSnapshot(*c.registry), true
}

// Module looks address up in the cached snapshot (see Snapshot), for a
// caller deciding whether to make a call at all. False means no module
// answers to address — or that there is no snapshot yet; Snapshot tells the
// two apart, and Modules fetches one.
func (c *Client) Module(address string) (ModuleInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.registry == nil {
		return ModuleInfo{}, false
	}
	for _, m := range c.registry.Modules {
		if m.Address == address {
			return m, true
		}
	}
	return ModuleInfo{}, false
}

// Installed reports whether the cached snapshot has a module answering to
// address, running or not. Gate a feature on it before calling, e.g.
// Installed(weavewire.Clipboard.Address()).
func (c *Client) Installed(address string) bool {
	_, ok := c.Module(address)
	return ok
}

// Running reports whether the cached snapshot has a module answering to
// address in the running state.
func (c *Client) Running(address string) bool {
	m, ok := c.Module(address)
	return ok && m.State == ModuleStateRunning
}

// routeModules applies a snapshot from the read loop and answers the
// Modules call it was for, if any.
func (c *Client) routeModules(env hvchannel.Envelope) {
	var snap ModulesSnapshot
	if err := json.Unmarshal(env.Data, &snap); err != nil {
		c.log.Warn("weaveclient: undecodable module snapshot", "kind", env.Kind, "err", err)
		c.answer(env.ID, pendingReply{
			err: fmt.Errorf("weaveclient: decoding %s: %w", env.Kind, err),
		})
		return
	}
	c.sawRegistry()
	kept := c.applyModules(snap)
	if env.Kind == hvchannel.KindModulesListResult {
		c.answer(env.ID, pendingReply{snap: &kept})
	}
}

// applyModules keeps snap if it is newer than the client's, runs the change
// handlers if so, and returns whichever snapshot is kept.
func (c *Client) applyModules(snap ModulesSnapshot) ModulesSnapshot {
	if snap.Modules == nil {
		snap.Modules = []ModuleInfo{}
	}
	c.mu.Lock()
	if c.registry != nil && c.registry.Revision >= snap.Revision {
		kept := *c.registry
		c.mu.Unlock()
		return kept
	}
	c.registry = &snap
	handlers := slices.Clone(c.moduleHandlers)
	c.mu.Unlock()
	for _, fn := range handlers {
		fn(cloneSnapshot(snap))
	}
	return snap
}

// resetModules forgets the snapshot, for a new authentication: the core on
// the other end may be a new process whose revisions restart from 1 — or a
// different weave-agent altogether, so what the wire showed about the old one
// is dropped too, back to what Options.CoreVersion says.
func (c *Client) resetModules() {
	c.mu.Lock()
	c.registry = nil
	c.coreRegistry = c.baseRegistry
	c.mu.Unlock()
}

// refreshModules fetches a snapshot after authenticating, for the cache and
// the change handlers; a core that does not answer is left alone. It closes
// done when it ends, for exec input waiting to learn what core is.
// ctx carries Authenticate's values but not its deadline, which has done its
// job; Options.RegistryTimeout and the channel's end bound the wait.
func (c *Client) refreshModules(ctx context.Context, done chan struct{}) {
	defer close(done)
	if _, err := c.Modules(ctx); err != nil {
		c.log.Debug("weaveclient: no module registry after authenticating", "err", err)
	}
}

// routeDeliveryFailed fails whatever the undeliverable frame's id names: a
// waiting call, or an exec session whose input could not be delivered.
func (c *Client) routeDeliveryFailed(env hvchannel.Envelope) {
	var df hvchannel.DeliveryFailed
	if err := json.Unmarshal(env.Data, &df); err != nil {
		c.log.Warn("weaveclient: undecodable delivery.failed", "err", err)
	}
	c.sawRegistry()
	derr := &DeliveryError{
		Module: df.Module, Kind: df.Kind, Reason: df.Reason, State: df.State, Detail: df.Detail,
	}
	if c.answer(env.ID, pendingReply{err: derr}) {
		return
	}
	if s := c.execSession(env.ID); s != nil {
		s.undeliverable(derr)
		return
	}
	c.log.Debug("weaveclient: undeliverable frame with nobody waiting",
		"id", env.ID, "module", df.Module, "kind", df.Kind, "reason", df.Reason)
}

// answer hands r to the call waiting on id, reporting whether there was one.
func (c *Client) answer(id string, r pendingReply) bool {
	if id == "" {
		return false
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	c.mu.Unlock()
	if ok {
		c.deliver(ch, r)
	}
	return ok
}

func cloneSnapshot(s ModulesSnapshot) ModulesSnapshot {
	s.Modules = slices.Clone(s.Modules)
	return s
}
