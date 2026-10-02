// Package weavetime is the time capability: read the guest's clock, and
// correct it after the host has suspended or snapshotted the VM.
package weavetime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Backend sets the guest OS's wall clock. Reading it is portable, so a
// backend only implements the part that needs a privileged syscall.
type Backend interface {
	SetTime(ctx context.Context, t time.Time) error
}

// Service is the time capability over an OS backend.
type Service struct{ b Backend }

// NewService builds the time service.
func NewService(b Backend) *Service { return &Service{b: b} }

// Capability implements weavemodule.Service.
func (s *Service) Capability() weavewire.Capability { return weavewire.Time }

// Register implements weavemodule.Service.
func (s *Service) Register(r *weavemodule.Registrar) error {
	r.Handle(weavewire.KindTimeGet, s.handleGet)
	r.Handle(weavewire.KindTimeSet, s.handleSet)
	return nil
}

// handleTimeGet reports the guest's clock without changing it, so a host can
// measure drift before deciding to correct it.
func (s *Service) handleGet(_ context.Context, _ []byte) ([]byte, error) {
	return weavewire.EncodePayload(readClock())
}

func readClock() weavewire.TimeResponse {
	now := time.Now()
	zone, offset := now.Zone()
	return weavewire.TimeResponse{
		UnixNano:      now.UTC().UnixNano(),
		Zone:          zone,
		OffsetSeconds: offset,
	}
}

// handleTimeSet corrects the guest's clock.
//
// This exists because a suspended or snapshotted VM resumes with the clock it
// stopped with and nothing inside notices: certificates fail validation,
// scheduled work fires late or all at once, and the guest's logs no longer line
// up with the host's. Only the host knows how long the guest was away.
func (s *Service) handleSet(ctx context.Context, payload []byte) ([]byte, error) {
	var req weavewire.TimeSetRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("weavetime: decoding set: %w", err)
	}
	if req.UnixNano == 0 {
		return nil, errNoTime
	}

	target := time.Unix(0, req.UnixNano).UTC()
	before := time.Now().UTC()
	skew := target.Sub(before)

	// The guard is against the HOST being wrong. A host whose own clock is
	// broken would otherwise drag a healthy guest with it, and the guest is in
	// no position to know which of them is right — so the caller states the
	// correction it considers plausible and anything larger is refused.
	if req.MaxSkewSeconds > 0 {
		limit := time.Duration(req.MaxSkewSeconds) * time.Second
		if skew > limit || skew < -limit {
			return nil, fmt.Errorf("%w: a %s correction is over the %s limit the host allowed",
				ErrSkewRefused, skew.Round(time.Second), limit)
		}
	}

	if err := s.b.SetTime(ctx, target); err != nil {
		return nil, fmt.Errorf("weavetime: setting the clock: %w", err)
	}
	return weavewire.EncodePayload(weavewire.TimeSetResponse{
		PreviousUnixNano: before.UnixNano(),
		SkewNanos:        int64(skew),
	})
}

var errNoTime = errors.New("weavetime: no time given to set")

// ErrSkewRefused reports a correction larger than the host itself allowed.
var ErrSkewRefused = errors.New("weavetime: refusing")
