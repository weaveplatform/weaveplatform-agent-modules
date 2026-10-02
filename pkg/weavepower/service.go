// Package weavepower is the power capability: graceful shutdown and restart
// from inside the guest.
//
// This is the feature that motivated an in-guest agent at all: a Windows guest
// does not act on the ACPI power button, so without a path from inside, the
// only way to end one is to cut it off mid-write and leave its filesystem and
// registry dirty.
package weavepower

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveagent"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Action is a power operation that has been decided but not yet performed.
//
// The split is the ordering contract. Run terminates the operating system that
// is running this agent, so it must not happen until the reply is on the wire —
// the dispatcher's DeferredHandler path guarantees that. Command describes what
// Run will do, so a shutdown that is accepted and then does nothing can be
// diagnosed from outside a guest nobody can log into.
type Action struct {
	Command string
	Run     func() error
}

// Backend asks the guest OS to power itself off or reboot, in the orderly way
// it would if a user chose it from the desktop. Each per-OS module supplies
// one; Unix serves macOS and Linux.
type Backend interface {
	Shutdown(ctx context.Context, reason string) (Action, error)
	Restart(ctx context.Context, reason string) (Action, error)
}

// Service is the power capability over an OS backend.
type Service struct{ b Backend }

// NewService builds the power service.
func NewService(b Backend) *Service { return &Service{b: b} }

// Capability implements weavemodule.Service.
func (s *Service) Capability() weavewire.Capability { return weavewire.Power }

// Register implements weavemodule.Service.
func (s *Service) Register(r *weavemodule.Registrar) error {
	log := r.Log()
	r.HandleDeferred(weavewire.KindPowerShutdown, handler(log, s.b.Shutdown))
	r.HandleDeferred(weavewire.KindPowerRestart, handler(log, s.b.Restart))
	return nil
}

// handler adapts a backend's Shutdown or Restart into a DeferredHandler.
//
// The shape is the point. The backend DECIDES what to do and returns it
// unperformed; this returns the acknowledgement; the dispatcher puts that on
// the wire; and only then is the action run. The command that follows
// terminates the OS running this agent, so a reply written afterwards would
// race the process being killed and the host would see a dead channel rather
// than an answer — and a host that cannot tell "accepted" from "channel died"
// falls back to cutting the VM off mid-write, which is exactly what the
// graceful path exists to avoid.
func handler(
	log *slog.Logger,
	decide func(ctx context.Context, reason string) (Action, error),
) weaveagent.DeferredHandler {
	return func(ctx context.Context, payload []byte) ([]byte, func(), error) {
		var req weavewire.PowerRequest
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, nil, fmt.Errorf("weavepower: decoding request: %w", err)
			}
		}

		action, err := decide(ctx, req.Reason)
		if err != nil {
			return nil, nil, err
		}

		resp, err := weavewire.EncodePayload(weavewire.PowerResponse{
			Accepted: true,
			Command:  action.Command,
		})
		if err != nil {
			return nil, nil, err
		}

		after := func() {
			if err := action.Run(); err != nil {
				// Nothing to reply to any more — the host has its
				// acknowledgement and is now waiting for the guest to go away.
				// The guest's own log is the only place this can surface, which
				// is why Command was reported before we got here.
				log.Error("weave: power command failed", "command", action.Command, "err", err)
			}
		}
		return resp, after, nil
	}
}
