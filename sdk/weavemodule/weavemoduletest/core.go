package weavemoduletest

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
)

// Core plays core's guest end of the hypervisor channel: it owns the guest
// side of the byte stream, routes each inbound envelope to the module whose
// address it names, and frames what modules send under their own address.
// It is the same shape as agent-core's HypervisorPeer — one reader, one write
// lock — including the authentication gate when a trusted key is given.
//
// Like weave-agent v0.9.2 it keeps a module registry — every served module,
// running and healthy, plus whatever a test adds with SetModule — answers
// modules.list, pushes modules.changed to an authenticated host, echoes
// envelope ids on its control replies, and answers a frame it cannot deliver
// with delivery.failed. Set Legacy for a core from before all that.
type Core struct {
	// Legacy makes the core behave as weave-agent before v0.9.2: it ignores
	// modules.list, never sends modules.changed or delivery.failed, and does
	// not echo envelope ids. Set it before Run.
	Legacy bool

	conn io.ReadWriteCloser
	r    *bufio.Reader
	wmu  sync.Mutex
	w    *bufio.Writer

	trusted ed25519.PublicKey
	authed  bool
	nonce   []byte

	mu      sync.Mutex
	modules map[string]chan modulesdk.Message
	// registry is what modules.list reports, by module id; revision moves
	// with every change. busy names addresses whose queue is full.
	registry map[string]hvchannel.ModuleInfo
	revision uint64
	busy     map[string]bool
	running  bool
}

// NewCore builds a core over the guest end of a channel. A nil trusted key
// disables the authentication gate, as for a test that is not about it; a
// non-nil one enforces it exactly as core does.
func NewCore(conn io.ReadWriteCloser, trusted ed25519.PublicKey) *Core {
	return &Core{
		conn:     conn,
		r:        bufio.NewReader(conn),
		w:        bufio.NewWriter(conn),
		trusted:  trusted,
		authed:   trusted == nil,
		modules:  make(map[string]chan modulesdk.Message),
		registry: make(map[string]hvchannel.ModuleInfo),
		busy:     make(map[string]bool),
	}
}

// Serve starts svc in a module registered under its capability's address and
// stops it when the test ends. Call it before Run.
func (c *Core) Serve(tb testing.TB, svc weavemodule.Service) *weavemodule.Module {
	tb.Helper()
	m := weavemodule.New(weavemodule.ModuleID("test", svc.Capability()), svc)
	in := make(chan modulesdk.Message, 64)
	c.mu.Lock()
	c.modules[m.Address()] = in
	c.mu.Unlock()
	c.SetModule(hvchannel.ModuleInfo{
		ID:           m.ID(),
		Version:      "0.0.0-test",
		Protocol:     modulesdk.Protocol,
		Address:      m.Address(),
		Capabilities: []string{},
		Privilege:    "system",
		Session:      "system",
		State:        "running",
		Health:       hvchannel.ModuleHealth{Status: hvchannel.HealthHealthy},
	})

	h := &coreHost{Host: NewHost(nil), tr: &coreTransport{core: c, address: m.Address(), in: in}}
	if err := m.Init(context.Background(), h); err != nil {
		tb.Fatalf("init %s: %v", m.Address(), err)
	}
	if err := m.Start(context.Background()); err != nil {
		tb.Fatalf("start %s: %v", m.Address(), err)
	}
	tb.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m
}

// SetModule adds m to the registry, or replaces the entry with its ID, and
// pushes the new snapshot to an authenticated host. An entry whose address
// no served module answers to stands for a module that is installed but not
// running: a frame for it fails with not_running and the entry's State and
// Detail.
func (c *Core) SetModule(m hvchannel.ModuleInfo) {
	if m.Capabilities == nil {
		m.Capabilities = []string{}
	}
	if m.Since == "" {
		m.Since = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if m.Health.Status == "" {
		m.Health.Status = hvchannel.HealthUnknown
	}
	c.mu.Lock()
	c.registry[m.ID] = m
	c.revision++
	c.mu.Unlock()
	c.pushModules()
}

// RemoveModule drops the entry with id from the registry and pushes the new
// snapshot. Frames for its address then fail with not_installed, unless a
// served module still answers to it.
func (c *Core) RemoveModule(id string) {
	c.mu.Lock()
	delete(c.registry, id)
	c.revision++
	c.mu.Unlock()
	c.pushModules()
}

// SetBusy marks address as a module whose receive queue is full, so a frame
// for it fails with busy, as core reports a module that is not keeping up.
func (c *Core) SetBusy(address string, busy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.busy[address] = busy
}

// Modules returns the registry as core reports it: sorted by id, at the
// current revision.
func (c *Core) Modules() hvchannel.ModulesSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

// PushModules sends the current snapshot as modules.changed, as core does
// on every change, to an authenticated host.
func (c *Core) PushModules() { c.pushModules() }

func (c *Core) snapshotLocked() hvchannel.ModulesSnapshot {
	out := hvchannel.ModulesSnapshot{Revision: c.revision, Modules: []hvchannel.ModuleInfo{}}
	for _, m := range c.registry {
		out.Modules = append(out.Modules, m)
	}
	slices.SortFunc(out.Modules, func(a, b hvchannel.ModuleInfo) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (c *Core) pushModules() {
	c.mu.Lock()
	push := c.running && c.authed && !c.Legacy
	snap := c.snapshotLocked()
	c.mu.Unlock()
	if push {
		c.reply(hvchannel.KindModulesChanged, "", snap)
	}
}

// Run reads the channel until it ends, then closes every module's inbound
// stream as core does when the channel drops.
func (c *Core) Run() {
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for addr, in := range c.modules {
			close(in)
			delete(c.modules, addr)
		}
	}()
	for {
		env, err := hvchannel.ReadEnvelope(c.r)
		if err != nil {
			// A frame that will not decode costs one message: the length
			// prefix kept the reader aligned. Anything else ends the channel.
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) {
				continue
			}
			return
		}
		if env.Module == hvchannel.ControlModule {
			c.control(env)
			continue
		}
		if !c.isAuthed() && !hvchannel.AllowedBeforeAuth(env.Kind) {
			c.refuse(env.ID)
			continue
		}
		c.deliver(env)
	}
}

// deliver hands env to its module, or says why it cannot — to an
// authenticated host only, as core does: which modules a guest has is not
// something the pre-auth hello exemption discloses.
func (c *Core) deliver(env hvchannel.Envelope) {
	c.mu.Lock()
	in := c.modules[env.Module]
	busy := c.busy[env.Module]
	var entry *hvchannel.ModuleInfo
	for _, m := range c.registry {
		if m.Address == env.Module {
			entry = &m
			break
		}
	}
	tell := c.authed && !c.Legacy
	c.mu.Unlock()

	failed := hvchannel.DeliveryFailed{Module: env.Module, Kind: env.Kind}
	switch {
	case busy:
		failed.Reason = hvchannel.ReasonBusy
	case in != nil:
		// Blocking, unlike core's queue: tests stream more than 64 chunks
		// and rely on the backpressure. SetBusy stands in for a full queue.
		in <- modulesdk.Message{Peer: modulesdk.PeerHypervisor, Kind: env.Kind, Data: env.Data}
		return
	case entry != nil:
		failed.Reason = hvchannel.ReasonNotRunning
		failed.State = entry.State
		failed.Detail = entry.Detail
	default:
		failed.Reason = hvchannel.ReasonNotInstalled
	}
	if tell {
		c.reply(hvchannel.KindDeliveryFailed, env.ID, failed)
	}
}

func (c *Core) isAuthed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authed
}

// refuse answers a frame an unauthenticated channel may not carry.
func (c *Core) refuse(id string) {
	c.reply(
		hvchannel.KindAuthResult,
		id,
		hvchannel.AuthResult{Reason: "channel is not authenticated"},
	)
}

// Close ends the guest side of the channel.
func (c *Core) Close() error { return c.conn.Close() } //nolint:wrapcheck // the test's own connection

func (c *Core) control(env hvchannel.Envelope) {
	id := env.ID
	switch env.Kind {
	case hvchannel.KindAuthBegin:
		nonce, err := hvchannel.NewNonce()
		if err != nil {
			return
		}
		c.nonce = nonce
		c.reply(hvchannel.KindAuthChallenge, id, hvchannel.AuthChallenge{Nonce: nonce})
	case hvchannel.KindAuthResponse:
		var resp hvchannel.AuthResponse
		if err := json.Unmarshal(env.Data, &resp); err != nil {
			c.reply(
				hvchannel.KindAuthResult,
				id,
				hvchannel.AuthResult{Reason: "malformed response"},
			)
			return
		}
		nonce := c.nonce
		c.nonce = nil
		reason, err := hvchannel.Verify(c.trusted, nonce, resp)
		if err != nil {
			c.reply(hvchannel.KindAuthResult, id, hvchannel.AuthResult{Reason: reason})
			return
		}
		c.mu.Lock()
		c.authed = true
		c.mu.Unlock()
		c.reply(hvchannel.KindAuthResult, id, hvchannel.AuthResult{OK: true})
	case hvchannel.KindModulesList:
		if c.Legacy {
			return // an older core does not know the kind and says nothing
		}
		if !c.isAuthed() {
			c.refuse(id)
			return
		}
		c.reply(hvchannel.KindModulesListResult, id, c.Modules())
	}
}

// reply sends one control frame, echoing the id of the frame it answers
// (none for an unsolicited one, or from a Legacy core).
func (c *Core) reply(kind, id string, payload any) {
	data, _ := json.Marshal(payload)
	if c.Legacy {
		id = ""
	}
	_ = c.write(hvchannel.Envelope{Module: hvchannel.ControlModule, Kind: kind, Data: data, ID: id})
}

func (c *Core) send(module, kind string, data []byte) error {
	return c.write(hvchannel.Envelope{Module: module, Kind: kind, Data: data})
}

func (c *Core) write(env hvchannel.Envelope) error {
	kind := env.Kind
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := hvchannel.WriteEnvelope(c.w, env); err != nil {
		return fmt.Errorf("weavemoduletest: writing %s: %w", kind, err)
	}
	if err := c.w.Flush(); err != nil {
		return fmt.Errorf("weavemoduletest: writing %s: %w", kind, err)
	}
	return nil
}

// coreTransport is one module's view of the core: sends are framed under the
// module's address, as core stamps them.
type coreTransport struct {
	core    *Core
	address string
	in      chan modulesdk.Message
}

func (t *coreTransport) Send(ctx context.Context, msg modulesdk.Message, _ bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := t.core.send(t.address, msg.Kind, msg.Data); err != nil {
		return false, err
	}
	return true, nil
}

func (t *coreTransport) Receive(
	context.Context,
) (<-chan modulesdk.Message, error) {
	return t.in, nil
}

type coreHost struct {
	*Host
	tr *coreTransport
}

func (h *coreHost) Transport() modulesdk.Transport { return h.tr }
