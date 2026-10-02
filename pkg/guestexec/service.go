package guestexec

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestpolicy"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// Service is the exec capability: it serves guestwire's exec ops with the
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

// Capability implements guestmodule.Service.
func (s *Service) Capability() guestwire.Capability { return guestwire.Exec }

// Register implements guestmodule.Service.
func (s *Service) Register(r *guestmodule.Registrar) error {
	host := r.Host()
	guard := &policyGuard{policy: host.Policy(), events: host.Events(), log: r.Log()}
	mgr := New(s.start, r.Emitter(), guard, r.Log())
	s.mu.Lock()
	s.mgr = mgr
	s.mu.Unlock()

	r.Handle(guestwire.KindExecStart, mgr.handleStart)
	r.Handle(guestwire.KindExecStdin, mgr.handleStdin)
	r.Handle(guestwire.KindExecResize, mgr.handleResize)
	r.Handle(guestwire.KindExecSignal, mgr.handleSignal)
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
	var req guestwire.ExecRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("guestexec: decoding start: %w", err)
	}
	resp, err := m.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	return guestwire.EncodePayload(resp)
}

// handleStdin takes a chunk of the process's input. It arrives without a
// correlation id, so nothing is replied — see guestwire.KindExecStdin.
func (m *Manager) handleStdin(_ context.Context, payload []byte) ([]byte, error) {
	var chunk guestwire.Chunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, fmt.Errorf("guestexec: decoding stdin: %w", err)
	}
	return nil, m.Stdin(chunk)
}

func (m *Manager) handleResize(_ context.Context, payload []byte) ([]byte, error) {
	var req guestwire.ExecResizeRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("guestexec: decoding resize: %w", err)
	}
	return nil, m.Resize(req)
}

func (m *Manager) handleSignal(_ context.Context, payload []byte) ([]byte, error) {
	var req guestwire.ExecSignalRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("guestexec: decoding signal: %w", err)
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
	req guestwire.ExecRequest,
) (Limits, error) {
	policy, present, err := g.current(ctx)
	if err != nil {
		// A policy that cannot be read or parsed refuses everything. Falling
		// back to allow-all here would turn a malformed document into an open
		// door, which is the one failure mode a policy must not have.
		g.audit(ctx, guestpolicy.ExecAudit{
			At: time.Now().UTC(), ExecID: execID, Argv: req.Argv, Dir: req.Dir,
			TTY: req.TTY, Refused: err.Error(), Policy: true,
		})
		return Limits{}, err
	}
	limits := Limits{MaxOutputBytes: policy.MaxOutputBytes}

	decision := policy.CheckExec(
		guestpolicy.ExecRequest{Argv: req.Argv, Dir: req.Dir, TTY: req.TTY},
	)
	record := guestpolicy.ExecAudit{
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
	record := guestpolicy.ExecAudit{At: time.Now().UTC(), ExecID: execID, ExitCode: &code}
	if cause != nil {
		record.Refused = cause.Error()
	}
	g.audit(ctx, record)
}

// current reads the module's policy, reporting whether a document was present
// at all — an absent policy permits everything, a present one is authoritative.
func (g *policyGuard) current(ctx context.Context) (guestpolicy.Policy, bool, error) {
	if g.policy == nil {
		return guestpolicy.Policy{}, false, nil
	}
	doc, err := g.policy.Get(ctx)
	if err != nil {
		// No policy has been delivered yet. That is the disconnected-guest
		// case, not a policy failure: permit and audit. A guest mid-setup has
		// never spoken to a policy server and is exactly when exec matters.
		g.log.Debug("guestweave: no exec policy delivered; permitting", "err", err)
		return guestpolicy.Policy{}, false, nil
	}
	policy, perr := guestpolicy.Parse(doc.Data)
	if perr != nil {
		return guestpolicy.Policy{}, true, perr
	}
	return policy, len(doc.Data) > 0, nil
}

func (g *policyGuard) audit(ctx context.Context, record guestpolicy.ExecAudit) {
	if g.events == nil {
		return
	}
	data, err := guestwire.EncodePayload(record)
	if err != nil {
		return
	}
	if err := g.events.Publish(ctx, guestpolicy.AuditTopic, data); err != nil {
		// The audit sink being unavailable must not stop the exec: refusing to
		// run because a log could not be written would make a broken event bus
		// look like a broken guest.
		g.log.Warn("guestweave: exec audit not published", "err", err)
	}
}
