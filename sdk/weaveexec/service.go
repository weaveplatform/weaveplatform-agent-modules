package weaveexec

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavepolicy"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Service is the exec capability: it serves weavewire's exec ops with the
// Starter the per-OS module supplies (UnixStarter on macOS and Linux,
// WindowsStarter on Windows). Policy, audit, streaming and exit accounting are
// the same on every OS.
type Service struct {
	start Starter

	mu  sync.Mutex
	mgr *Manager
}

// NewService builds the exec service around an OS starter.
func NewService(start Starter) *Service { return &Service{start: start} }

// Capability implements weavemodule.Service.
func (s *Service) Capability() weavewire.Capability { return weavewire.Exec }

// Register implements weavemodule.Service.
func (s *Service) Register(r *weavemodule.Registrar) error {
	host := r.Host()
	guard := &policyGuard{policy: host.Policy(), events: host.Events(), log: r.Log()}
	mgr := New(s.start, r.Emitter(), guard, r.Log())
	s.mu.Lock()
	s.mgr = mgr
	s.mu.Unlock()

	r.Handle(weavewire.KindExecStart, mgr.handleStart)
	r.Handle(weavewire.KindExecStdin, mgr.handleStdin)
	r.Handle(weavewire.KindExecResize, mgr.handleResize)
	r.Handle(weavewire.KindExecSignal, mgr.handleSignal)
	return nil
}

// Stop terminates every running exec.
//
// Killing them is deliberate. A process started on behalf of a host that can
// no longer be reached has nobody to report to, and leaving it running orphans
// it inside a guest whose agent has gone — precisely the leak core's own
// supervisor exists to prevent.
func (s *Service) Stop(context.Context) error {
	s.mu.Lock()
	mgr := s.mgr
	s.mu.Unlock()
	if mgr != nil {
		mgr.Stop()
	}
	return nil
}

func (m *Manager) handleStart(ctx context.Context, payload []byte) ([]byte, error) {
	var req weavewire.ExecRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("weaveexec: decoding start: %w", err)
	}
	resp, err := m.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	return weavewire.EncodePayload(resp)
}

// handleStdin takes a chunk of the process's input. It arrives without a
// correlation id, so nothing is replied — see weavewire.KindExecStdin.
func (m *Manager) handleStdin(_ context.Context, payload []byte) ([]byte, error) {
	var chunk weavewire.Chunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, fmt.Errorf("weaveexec: decoding stdin: %w", err)
	}
	return nil, m.Stdin(chunk)
}

func (m *Manager) handleResize(_ context.Context, payload []byte) ([]byte, error) {
	var req weavewire.ExecResizeRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("weaveexec: decoding resize: %w", err)
	}
	return nil, m.Resize(req)
}

func (m *Manager) handleSignal(_ context.Context, payload []byte) ([]byte, error) {
	var req weavewire.ExecSignalRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("weaveexec: decoding signal: %w", err)
	}
	return nil, m.Signal(req)
}

// policyGuard authorises execs against host-delivered policy and audits every
// one of them. It implements Guard.
//
// Policy is read per exec rather than cached at Init: a policy tightened while
// a guest is running must take effect on the next command, not at the next
// restart of an agent that may never restart.
type policyGuard struct {
	policy modulesdk.PolicyReader
	events modulesdk.Events
	log    *slog.Logger
}

// Authorise fetches the current policy, judges the request, and audits the
// decision — including refusals, which are the ones worth having a record of.
//
// It holds no per-exec state: several execs are authorised concurrently, so
// anything stashed on the guard between here and its use would belong to
// whichever exec asked last. Limits travel back with the decision instead.
func (g *policyGuard) Authorise(
	ctx context.Context,
	execID string,
	req weavewire.ExecRequest,
) (Limits, error) {
	policy, present, err := g.current(ctx)
	if err != nil {
		// A policy that cannot be read or parsed refuses everything. Falling
		// back to allow-all here would turn a malformed document into an open
		// door, which is the one failure mode a policy must not have.
		g.audit(ctx, weavepolicy.ExecAudit{
			At: time.Now().UTC(), ExecID: execID, Argv: req.Argv, Dir: req.Dir,
			TTY: req.TTY, Refused: err.Error(), Policy: true,
		})
		return Limits{}, err
	}
	limits := Limits{MaxOutputBytes: policy.MaxOutputBytes}

	decision := policy.CheckExec(
		weavepolicy.ExecRequest{Argv: req.Argv, Dir: req.Dir, TTY: req.TTY},
	)
	record := weavepolicy.ExecAudit{
		At: time.Now().UTC(), ExecID: execID, Argv: req.Argv, Dir: req.Dir,
		TTY: req.TTY, Policy: present,
	}
	if decision != nil {
		record.Refused = decision.Error()
	}
	// Audited BEFORE the process starts: a record written on completion misses
	// exactly the executions that mattered — the one that hung, the one that
	// took the guest down, the one still running when the channel dropped.
	g.audit(ctx, record)
	return limits, decision
}

// Finished records the outcome alongside the start record.
func (g *policyGuard) Finished(ctx context.Context, execID string, code int, cause error) {
	record := weavepolicy.ExecAudit{At: time.Now().UTC(), ExecID: execID, ExitCode: &code}
	if cause != nil {
		record.Refused = cause.Error()
	}
	g.audit(ctx, record)
}

// current reads the module's policy, reporting whether a document was present
// at all — an absent policy permits everything, a present one is authoritative.
func (g *policyGuard) current(ctx context.Context) (weavepolicy.Policy, bool, error) {
	if g.policy == nil {
		return weavepolicy.Policy{}, false, nil
	}
	doc, err := g.policy.Get(ctx)
	if err != nil {
		// No policy has been delivered yet. That is the disconnected-guest
		// case, not a policy failure: permit and audit. A guest mid-setup has
		// never spoken to a policy server and is exactly when exec matters.
		g.log.Debug("weave: no exec policy delivered; permitting", "err", err)
		return weavepolicy.Policy{}, false, nil
	}
	policy, perr := weavepolicy.Parse(doc.Data)
	if perr != nil {
		return weavepolicy.Policy{}, true, perr
	}
	return policy, len(doc.Data) > 0, nil
}

func (g *policyGuard) audit(ctx context.Context, record weavepolicy.ExecAudit) {
	if g.events == nil {
		return
	}
	data, err := weavewire.EncodePayload(record)
	if err != nil {
		return
	}
	if err := g.events.Publish(ctx, weavepolicy.AuditTopic, data); err != nil {
		// The audit sink being unavailable must not stop the exec: refusing to
		// run because a log could not be written would make a broken event bus
		// look like a broken guest.
		g.log.Warn("weave: exec audit not published", "err", err)
	}
}
