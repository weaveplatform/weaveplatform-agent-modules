// Package weavemetrics is the metrics capability: one live resource sample
// per request, at a cadence the host chooses.
package weavemetrics

import (
	"context"
	"fmt"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Backend takes one live resource sample. The backend fills what its OS can
// answer and leaves the rest zero; the portable fields are filled here.
type Backend interface {
	Sample(ctx context.Context, out *weavewire.MetricsResponse) error
}

// Service is the metrics capability over an OS backend.
type Service struct{ b Backend }

// NewService builds the metrics service.
func NewService(b Backend) *Service { return &Service{b: b} }

// Capability implements weavemodule.Service.
func (s *Service) Capability() weavewire.Capability { return weavewire.Metrics }

// Register implements weavemodule.Service.
func (s *Service) Register(r *weavemodule.Registrar) error {
	r.Handle(weavewire.KindMetricsSample, s.handleSample)
	return nil
}

// handleMetricsSample takes one reading. The portable fields are filled here
// and the backend adds what only its OS can answer.
func (s *Service) handleSample(ctx context.Context, _ []byte) ([]byte, error) {
	sample := weavewire.MetricsResponse{SampledAt: time.Now().UTC()}
	if err := s.b.Sample(ctx, &sample); err != nil {
		return nil, fmt.Errorf("weavemetrics: sampling: %w", err)
	}
	return weavewire.EncodePayload(sample)
}
