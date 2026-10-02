package weavetime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavetime"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// recordingClock captures what the module asked the OS to do, without
// touching the machine's real clock.
type recordingClock struct {
	set []time.Time
	err error
}

func (c *recordingClock) SetTime(_ context.Context, t time.Time) error {
	if c.err != nil {
		return c.err
	}
	c.set = append(c.set, t)
	return nil
}

func call(t *testing.T, clock *recordingClock, kind string, payload any) weavewire.Result {
	t.Helper()
	return weavemoduletest.Start(t, weavetime.NewService(clock)).Call(kind, payload)
}

func TestTimeSetAppliesAndReportsTheSkew(t *testing.T) {
	clock := &recordingClock{}
	target := time.Now().UTC().Add(90 * time.Second)

	res := call(
		t,
		clock,
		weavewire.KindTimeSet,
		weavewire.TimeSetRequest{UnixNano: target.UnixNano()},
	)
	if res.Err != "" {
		t.Fatalf("set failed: %s", res.Err)
	}

	if len(clock.set) != 1 {
		t.Fatalf("the OS clock was set %d times", len(clock.set))
	}
	if !clock.set[0].Equal(target) {
		t.Errorf("set to %v, want %v", clock.set[0], target)
	}

	var out weavewire.TimeSetResponse
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	// The skew is the number an operator alerts on, so it has to be roughly
	// right rather than merely present.
	if skew := out.Skew(); skew < 85*time.Second || skew > 95*time.Second {
		t.Errorf("reported skew %v, want about 90s", skew)
	}
	if out.PreviousUnixNano == 0 {
		t.Error("the clock before the change was not recorded")
	}
}

// The skew guard exists because the HOST may be the one that is wrong. A guest
// cannot tell which of them is right, so it refuses corrections the host itself
// called implausible.
func TestTimeSetRefusesACorrectionBeyondTheHostsOwnLimit(t *testing.T) {
	clock := &recordingClock{}
	wayOff := time.Now().UTC().Add(48 * time.Hour)

	res := call(t, clock, weavewire.KindTimeSet, weavewire.TimeSetRequest{
		UnixNano:       wayOff.UnixNano(),
		MaxSkewSeconds: 60,
	})
	if res.Err == "" {
		t.Fatal("a 48-hour correction was accepted under a 60-second limit")
	}
	if !strings.Contains(res.Err, "refusing") {
		t.Errorf("unhelpful refusal: %s", res.Err)
	}
	if len(clock.set) != 0 {
		t.Fatal("the clock was changed despite the refusal")
	}
}

// A correction inside the limit must still go through — the guard must not be
// so eager that it blocks the ordinary case.
func TestTimeSetAllowsACorrectionInsideTheLimit(t *testing.T) {
	clock := &recordingClock{}
	target := time.Now().UTC().Add(10 * time.Second)

	res := call(t, clock, weavewire.KindTimeSet, weavewire.TimeSetRequest{
		UnixNano:       target.UnixNano(),
		MaxSkewSeconds: 60,
	})
	if res.Err != "" {
		t.Fatalf("a 10s correction was refused under a 60s limit: %s", res.Err)
	}
	if len(clock.set) != 1 {
		t.Fatal("the clock was not set")
	}
}

// Going BACKWARDS is the common case after a host has been suspended, so a
// negative skew must be allowed by the same limit that allows a positive one.
func TestTimeSetAllowsGoingBackwards(t *testing.T) {
	clock := &recordingClock{}
	target := time.Now().UTC().Add(-10 * time.Second)

	res := call(t, clock, weavewire.KindTimeSet, weavewire.TimeSetRequest{
		UnixNano:       target.UnixNano(),
		MaxSkewSeconds: 60,
	})
	if res.Err != "" {
		t.Fatalf("a backwards correction was refused: %s", res.Err)
	}
	var out weavewire.TimeSetResponse
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Skew() > 0 {
		t.Errorf("skew = %v, want negative for a clock set backwards", out.Skew())
	}
}

func TestTimeGetDoesNotChangeTheClock(t *testing.T) {
	clock := &recordingClock{}
	res := call(t, clock, weavewire.KindTimeGet, nil)
	if res.Err != "" {
		t.Fatal(res.Err)
	}
	var out weavewire.TimeResponse
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.UnixNano == 0 {
		t.Error("no clock reading")
	}
	if len(clock.set) != 0 {
		t.Fatal("reading the clock changed it")
	}
}

func TestServesExactlyTheTimeContract(t *testing.T) {
	if err := weavemodule.CheckParity(weavetime.NewService(&recordingClock{})); err != nil {
		t.Fatal(err)
	}
}

func TestTimeSetRefusesAMissingTime(t *testing.T) {
	clock := &recordingClock{}
	if res := call(t, clock, weavewire.KindTimeSet, weavewire.TimeSetRequest{}); res.Err == "" {
		t.Fatal("a request with no time was applied")
	}
	if res := call(t, clock, weavewire.KindTimeSet, "garbage"); res.Err == "" {
		t.Fatal("a malformed request was applied")
	}
	if len(clock.set) != 0 {
		t.Fatal("the clock was changed")
	}
}

// The OS refusing (no privilege to set the clock) must reach the host as the
// guest's error, not as success.
func TestTimeSetReportsTheBackendRefusing(t *testing.T) {
	clock := &recordingClock{err: errors.New("operation not permitted")}
	res := call(
		t,
		clock,
		weavewire.KindTimeSet,
		weavewire.TimeSetRequest{UnixNano: time.Now().UnixNano()},
	)
	if !strings.Contains(res.Err, "not permitted") {
		t.Fatalf("err = %q", res.Err)
	}
}
