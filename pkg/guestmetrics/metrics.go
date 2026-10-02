// Package guestmetrics is the metrics capability: one live resource sample
// per request, at a cadence the host chooses.
package guestmetrics

import (
	"context"
	"fmt"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

// Backend takes one live resource sample. The backend fills what its OS can
// answer and leaves the rest zero; the portable fields are filled here.
type Backend interface {
	Sample(ctx context.Context, out *guestwire.MetricsResponse) error
}

// Service is the metrics capability over an OS backend.
type Service struct{ b Backend }

// NewService builds the metrics service.
func NewService(b Backend) *Service { return &Service{b: b} }

// Capability implements guestmodule.Service.
func (s *Service) Capability() guestwire.Capability { return guestwire.Metrics }

// Register implements guestmodule.Service.
func (s *Service) Register(r *guestmodule.Registrar) error {
	r.Handle(guestwire.KindMetricsSample, s.handleSample)
	return nil
}

// handleMetricsSample takes one reading. The portable fields are filled here
// and the backend adds what only its OS can answer.
func (s *Service) handleSample(ctx context.Context, _ []byte) ([]byte, error) {
	sample := guestwire.MetricsResponse{SampledAt: time.Now().UTC()}
	if err := s.b.Sample(ctx, &sample); err != nil {
		return nil, fmt.Errorf("guestmetrics: sampling: %w", err)
	}
	return guestwire.EncodePayload(sample)
}
