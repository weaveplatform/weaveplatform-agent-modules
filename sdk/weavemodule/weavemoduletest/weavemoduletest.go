// Package weavemoduletest runs weave capability services the way core
// would, without core: an in-memory Host and Transport for driving one
// service's ops, and a Core that serves several capability modules over a real
// hypervisor-channel byte stream for host-to-guest loopback tests.
//
// Per-OS modules use it to test their backends through the real dispatch path;
// sdk/weaveclient uses Core to test the host client against real framing.
package weavemoduletest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Transport is an in-memory modulesdk.Transport. Inbound messages are pushed
// with Deliver; everything the module sends is recorded.
//
// Send refuses a cancelled context, as core's gRPC transport does. A double
// that ignored it would report delivered a message production never sends —
// the bug that once left `weave exec` waiting for an exit event forever.
type Transport struct {
	// OnSend, when set, runs before a message is recorded. A non-nil error
	// fails the send and the message is not recorded.
	OnSend func(msg modulesdk.Message) error

	in      chan modulesdk.Message
	mu      sync.Mutex
	sent    []modulesdk.Message
	changed chan struct{}
}

// NewTransport builds an empty transport.
func NewTransport() *Transport {
	return &Transport{in: make(chan modulesdk.Message, 64), changed: make(chan struct{}, 1)}
}

// Send implements modulesdk.Transport.
func (t *Transport) Send(ctx context.Context, msg modulesdk.Message, _ bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if t.OnSend != nil {
		if err := t.OnSend(msg); err != nil {
			return false, err
		}
	}
	t.mu.Lock()
	t.sent = append(t.sent, msg)
	t.mu.Unlock()
	select {
	case t.changed <- struct{}{}:
	default:
	}
	return true, nil
}

// Receive implements modulesdk.Transport.
func (t *Transport) Receive(context.Context) (<-chan modulesdk.Message, error) { return t.in, nil }

// Deliver pushes one inbound message to the module.
func (t *Transport) Deliver(msg modulesdk.Message) { t.in <- msg }

// Sent returns a copy of every message sent so far, in order.
func (t *Transport) Sent() []modulesdk.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]modulesdk.Message(nil), t.sent...)
}

// WaitFor blocks until cond holds over the sent messages or timeout passes,
// reporting which.
func (t *Transport) WaitFor(timeout time.Duration, cond func([]modulesdk.Message) bool) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if cond(t.Sent()) {
			return true
		}
		select {
		case <-t.changed:
		case <-deadline.C:
			return cond(t.Sent())
		}
	}
}

// Host is an in-memory modulesdk.Host. Policy is absent unless set, and
// published events are recorded.
type Host struct {
	T *Transport

	mu        sync.Mutex
	policy    *modulesdk.PolicyDocument
	policyErr error
	published []Published
	publishFn func(topic string) error
}

// Published is one event a module published.
type Published struct {
	Topic string
	Data  []byte
}

// NewHost builds a host around t.
func NewHost(t *Transport) *Host { return &Host{T: t} }

// SetPolicy delivers a policy document; err, when non-nil, is what Get
// returns instead (no policy delivered yet).
func (h *Host) SetPolicy(data []byte, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.policy = &modulesdk.PolicyDocument{Revision: 1, Data: data}
	h.policyErr = err
}

// FailPublish makes Publish fail with the error fn returns.
func (h *Host) FailPublish(fn func(topic string) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.publishFn = fn
}

// Published returns the events published so far.
func (h *Host) Published() []Published {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Published(nil), h.published...)
}

// Transport implements modulesdk.Host.
func (h *Host) Transport() modulesdk.Transport { return h.T }

// Log implements modulesdk.Host; output is discarded.
func (h *Host) Log() *slog.Logger { return slog.New(slog.DiscardHandler) }

// Identity implements modulesdk.Host; guest modules have none.
func (h *Host) Identity() modulesdk.Identity { return nil }

// Store implements modulesdk.Host; guest modules keep no state.
func (h *Host) Store(string) modulesdk.Store { return nil }

// UI implements modulesdk.Host; guest modules declare no surfaces.
func (h *Host) UI() modulesdk.UIBroker { return nil }

// Policy implements modulesdk.Host: nil until SetPolicy is called, as on a
// guest that has never been sent one.
func (h *Host) Policy() modulesdk.PolicyReader {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.policy == nil {
		return nil
	}
	return policyReader{h}
}

// Events implements modulesdk.Host.
func (h *Host) Events() modulesdk.Events { return events{h} }

type policyReader struct{ h *Host }

func (p policyReader) Get(context.Context) (modulesdk.PolicyDocument, error) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	if p.h.policyErr != nil {
		return modulesdk.PolicyDocument{}, p.h.policyErr
	}
	return *p.h.policy, nil
}

func (p policyReader) Watch(context.Context) (<-chan modulesdk.PolicyDocument, error) {
	return nil, errNotSupported
}

type events struct{ h *Host }

func (e events) Publish(_ context.Context, topic string, data []byte) error {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	if e.h.publishFn != nil {
		if err := e.h.publishFn(topic); err != nil {
			return err
		}
	}
	e.h.published = append(e.h.published, Published{Topic: topic, Data: data})
	return nil
}

func (e events) Subscribe(context.Context, ...string) (<-chan modulesdk.Event, error) {
	return nil, errNotSupported
}

var errNotSupported = errors.New("weavemoduletest: not supported")

// Harness is one service running in a started module over an in-memory host.
type Harness struct {
	Module *weavemodule.Module
	Host   *Host
	T      *Transport
	// Timeout bounds how long Call waits for a result. Start sets 10s.
	Timeout time.Duration

	tb     testing.TB
	nextID atomic.Uint64
}

// Start initialises and starts svc in a module, stopping it when the test
// ends. setup, when given, runs on the host before Init (to set a policy, or
// a failing send hook).
func Start(tb testing.TB, svc weavemodule.Service, setup ...func(*Host)) *Harness {
	tb.Helper()
	tr := NewTransport()
	host := NewHost(tr)
	for _, fn := range setup {
		fn(host)
	}
	m := weavemodule.New(weavemodule.ModuleID("test", svc.Capability()), svc)
	if err := m.Init(context.Background(), host); err != nil {
		tb.Fatalf("init: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		tb.Fatalf("start: %v", err)
	}
	tb.Cleanup(func() { _ = m.Stop(context.Background()) })
	return &Harness{Module: m, Host: host, T: tr, Timeout: 10 * time.Second, tb: tb}
}

// Notify delivers a command with no correlation id, as a stream's host-to-guest
// half is sent.
func (h *Harness) Notify(kind string, payload any) {
	h.tb.Helper()
	h.deliver(kind, "", payload)
}

// Call delivers a command and waits for its result.
func (h *Harness) Call(kind string, payload any) weavewire.Result {
	h.tb.Helper()
	id := "call-" + strconv.FormatUint(h.nextID.Add(1), 10)
	h.deliver(kind, id, payload)
	var res weavewire.Result
	found := h.T.WaitFor(h.Timeout, func(sent []modulesdk.Message) bool {
		for _, m := range sent {
			if m.Kind != weavewire.ResultKind(kind) {
				continue
			}
			var r weavewire.Result
			if json.Unmarshal(m.Data, &r) == nil && r.ID == id {
				res = r
				return true
			}
		}
		return false
	})
	if !found {
		h.tb.Fatalf("no %s result for %s", kind, id)
	}
	return res
}

// Decode calls kind and decodes a successful result's payload into out,
// failing the test on a guest error.
func (h *Harness) Decode(kind string, payload, out any) {
	h.tb.Helper()
	res := h.Call(kind, payload)
	if res.Err != "" {
		h.tb.Fatalf("%s: %s", kind, res.Err)
	}
	if err := json.Unmarshal(res.Payload, out); err != nil {
		h.tb.Fatalf("%s: decoding: %v", kind, err)
	}
}

func (h *Harness) deliver(kind, id string, payload any) {
	h.tb.Helper()
	data, err := weavewire.EncodeCommand(id, payload)
	if err != nil {
		h.tb.Fatalf("encoding %s: %v", kind, err)
	}
	h.T.Deliver(modulesdk.Message{Peer: modulesdk.PeerHypervisor, Kind: kind, Data: data})
}
