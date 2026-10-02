package guestmetrics_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmetrics"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule/guestmoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

type stubMetrics struct{ err error }

func (s stubMetrics) Sample(_ context.Context, out *guestwire.MetricsResponse) error {
	if s.err != nil {
		return s.err
	}
	out.CPUPercent = 12.5
	out.MemoryTotalBytes = 1 << 30
	return nil
}

func TestServesExactlyTheMetricsContract(t *testing.T) {
	if err := guestmodule.CheckParity(guestmetrics.NewService(stubMetrics{})); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsSampleCarriesTheBackendsReading(t *testing.T) {
	h := guestmoduletest.Start(t, guestmetrics.NewService(stubMetrics{}))
	var out guestwire.MetricsResponse
	h.Decode(guestwire.KindMetricsSample, nil, &out)
	if out.CPUPercent != 12.5 || out.MemoryTotalBytes != 1<<30 {
		t.Fatalf("backend reading lost: %+v", out)
	}
	// The timestamp is the portable half: the backend never sets it.
	if out.SampledAt.IsZero() {
		t.Error("no sample timestamp")
	}
}

func TestMetricsBackendFailureReachesTheHost(t *testing.T) {
	h := guestmoduletest.Start(
		t,
		guestmetrics.NewService(stubMetrics{err: errors.New("no procfs")}),
	)
	if res := h.Call(guestwire.KindMetricsSample, nil); !strings.Contains(res.Err, "no procfs") {
		t.Fatalf("err = %q", res.Err)
	}
}
