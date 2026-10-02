// Package guestpower is the power capability: graceful shutdown and restart
// from inside the guest.
//
// This is the feature that motivated an in-guest agent at all: a Windows guest
// does not act on the ACPI power button, so without a path from inside, the
// only way to end one is to cut it off mid-write and leave its filesystem and
// registry dirty.
package guestpower

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestagent"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
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

// Capability implements guestmodule.Service.
func (s *Service) Capability() guestwire.Capability { return guestwire.Power }

// Register implements guestmodule.Service.
func (s *Service) Register(r *guestmodule.Registrar) error {
	log := r.Log()
	r.HandleDeferred(guestwire.KindPowerShutdown, handler(log, s.b.Shutdown))
	r.HandleDeferred(guestwire.KindPowerRestart, handler(log, s.b.Restart))
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
) guestagent.DeferredHandler {
	return func(ctx context.Context, payload []byte) ([]byte, func(), error) {
		var req guestwire.PowerRequest
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, nil, fmt.Errorf("guestpower: decoding request: %w", err)
			}
		}

		action, err := decide(ctx, req.Reason)
		if err != nil {
			return nil, nil, err
		}

		resp, err := guestwire.EncodePayload(guestwire.PowerResponse{
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
				log.Error("guestweave: power command failed", "command", action.Command, "err", err)
			}
		}
		return resp, after, nil
	}
}
