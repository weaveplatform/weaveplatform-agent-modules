package weavemetrics_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemetrics"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

type stubMetrics struct{ err error }

func (s stubMetrics) Sample(_ context.Context, out *weavewire.MetricsResponse) error {
	if s.err != nil {
		return s.err
	}
	out.CPUPercent = 12.5
	out.MemoryTotalBytes = 1 << 30
	return nil
}

func TestServesExactlyTheMetricsContract(t *testing.T) {
	if err := weavemodule.CheckParity(weavemetrics.NewService(stubMetrics{})); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsSampleCarriesTheBackendsReading(t *testing.T) {
	h := weavemoduletest.Start(t, weavemetrics.NewService(stubMetrics{}))
	var out weavewire.MetricsResponse
	h.Decode(weavewire.KindMetricsSample, nil, &out)
	if out.CPUPercent != 12.5 || out.MemoryTotalBytes != 1<<30 {
		t.Fatalf("backend reading lost: %+v", out)
	}
	// The timestamp is the portable half: the backend never sets it.
	if out.SampledAt.IsZero() {
		t.Error("no sample timestamp")
	}
}

func TestMetricsBackendFailureReachesTheHost(t *testing.T) {
	h := weavemoduletest.Start(
		t,
		weavemetrics.NewService(stubMetrics{err: errors.New("no procfs")}),
	)
	if res := h.Call(weavewire.KindMetricsSample, nil); !strings.Contains(res.Err, "no procfs") {
		t.Fatalf("err = %q", res.Err)
	}
}
