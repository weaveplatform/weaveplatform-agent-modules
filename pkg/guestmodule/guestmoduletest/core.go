package guestmoduletest

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule"
)

// Core plays core's guest end of the hypervisor channel: it owns the guest
// side of the byte stream, routes each inbound envelope to the module whose
// address it names, and frames what modules send under their own address.
// It is the same shape as agent-core's HypervisorPeer — one reader, one write
// lock — including the authentication gate when a trusted key is given.
type Core struct {
	conn io.ReadWriteCloser
	r    *bufio.Reader
	wmu  sync.Mutex
	w    *bufio.Writer

	trusted ed25519.PublicKey
	authed  bool
	nonce   []byte

	mu      sync.Mutex
	modules map[string]chan modulesdk.Message
}

// NewCore builds a core over the guest end of a channel. A nil trusted key
// disables the authentication gate, as for a test that is not about it; a
// non-nil one enforces it exactly as core does.
func NewCore(conn io.ReadWriteCloser, trusted ed25519.PublicKey) *Core {
	return &Core{
		conn:    conn,
		r:       bufio.NewReader(conn),
		w:       bufio.NewWriter(conn),
		trusted: trusted,
		authed:  trusted == nil,
		modules: make(map[string]chan modulesdk.Message),
	}
}

// Serve starts svc in a module registered under its capability's address and
// stops it when the test ends. Call it before Run.
func (c *Core) Serve(tb testing.TB, svc guestmodule.Service) *guestmodule.Module {
	tb.Helper()
	m := guestmodule.New(guestmodule.ModuleID("test", svc.Capability()), svc)
	in := make(chan modulesdk.Message, 64)
	c.mu.Lock()
	c.modules[m.Address()] = in
	c.mu.Unlock()

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

// Run reads the channel until it ends, then closes every module's inbound
// stream as core does when the channel drops.
func (c *Core) Run() {
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
		if !c.authed && !hvchannel.AllowedBeforeAuth(env.Kind) {
			c.reply(
				hvchannel.KindAuthResult,
				hvchannel.AuthResult{Reason: "channel is not authenticated"},
			)
			continue
		}
		c.mu.Lock()
		in := c.modules[env.Module]
		c.mu.Unlock()
		if in == nil {
			continue // no module answers to that address; core drops it too
		}
		in <- modulesdk.Message{Peer: modulesdk.PeerHypervisor, Kind: env.Kind, Data: env.Data}
	}
}

// Close ends the guest side of the channel.
func (c *Core) Close() error { return c.conn.Close() } //nolint:wrapcheck // the test's own connection

func (c *Core) control(env hvchannel.Envelope) {
	switch env.Kind {
	case hvchannel.KindAuthBegin:
		nonce, err := hvchannel.NewNonce()
		if err != nil {
			return
		}
		c.nonce = nonce
		c.reply(hvchannel.KindAuthChallenge, hvchannel.AuthChallenge{Nonce: nonce})
	case hvchannel.KindAuthResponse:
		var resp hvchannel.AuthResponse
		if err := json.Unmarshal(env.Data, &resp); err != nil {
			c.reply(hvchannel.KindAuthResult, hvchannel.AuthResult{Reason: "malformed response"})
			return
		}
		nonce := c.nonce
		c.nonce = nil
		reason, err := hvchannel.Verify(c.trusted, nonce, resp)
		if err != nil {
			c.reply(hvchannel.KindAuthResult, hvchannel.AuthResult{Reason: reason})
			return
		}
		c.authed = true
		c.reply(hvchannel.KindAuthResult, hvchannel.AuthResult{OK: true})
	}
}

func (c *Core) reply(kind string, payload any) {
	data, _ := json.Marshal(payload)
	_ = c.send(hvchannel.ControlModule, kind, data)
}

func (c *Core) send(module, kind string, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := hvchannel.WriteEnvelope(
		c.w,
		hvchannel.Envelope{Module: module, Kind: kind, Data: data},
	); err != nil {
		return fmt.Errorf("guestmoduletest: writing %s: %w", kind, err)
	}
	if err := c.w.Flush(); err != nil {
		return fmt.Errorf("guestmoduletest: writing %s: %w", kind, err)
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
