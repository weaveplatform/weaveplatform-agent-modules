package modulesdk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
)

// fakeModule records what the runtime calls and can be told to fail.
type fakeModule struct {
	initErr, startErr, stopErr error
	host                       Host
	stopCtx                    context.Context
	jobRuns                    atomic.Int32
	declare                    bool
}

func (m *fakeModule) ID() string             { return "fake" }
func (m *fakeModule) Requires() []Capability { return []Capability{"a.cap", "b.cap"} }
func (m *fakeModule) Init(_ context.Context, h Host) error {
	m.host = h
	if m.declare {
		if err := h.UI().Declare(Surface{ID: "card"}); err != nil {
			return err
		}
	}
	return m.initErr
}
func (m *fakeModule) Start(context.Context) error { return m.startErr }
func (m *fakeModule) Stop(ctx context.Context) error {
	m.stopCtx = ctx
	return m.stopErr
}

func (m *fakeModule) Health() Health {
	return Health{
		Status:  HealthDegraded,
		Reason:  "backend away",
		Details: map[string]string{"k": "v"},
	}
}

type scheduledModule struct{ fakeModule }

func (m *scheduledModule) Jobs() []Job {
	return []Job{{Name: "tick", Every: time.Hour, Run: func(context.Context) { m.jobRuns.Add(1) }}}
}

type configModule struct {
	fakeModule
	got []byte
	err error
}

func (m *configModule) SetConfig(doc []byte) error {
	m.got = doc
	return m.err
}

func newServer(t *testing.T, m Module) (*moduleServer, *fakeHost) {
	t.Helper()
	h := newFakeHost(t)
	return &moduleServer{
		m:              m,
		shutdown:       make(chan struct{}, 1),
		healthInterval: 30,
		connectHost:    connectTo(t, h),
	}, h
}

func connectTo(t *testing.T, h *fakeHost) func() (*hostClient, error) {
	return func() (*hostClient, error) { return h.dial(t), nil }
}

func initReq(id string) *agentv1.InitRequest {
	return &agentv1.InitRequest{ModuleId: id, Protocol: Protocol}
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("err = %v (code %v), want code %v", err, got, want)
	}
}

func TestLifecycleHappyPath(t *testing.T) {
	m := &scheduledModule{}
	m.declare = true
	s, _ := newServer(t, m)
	s.hostLost = make(chan struct{}, 1)
	ctx := ctxT(t)

	req := initReq("fake")
	req.WatchdogIntervalSeconds = 60
	resp, err := s.Init(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetRequires()) != 2 || resp.GetRequires()[1].GetName() != "b.cap" ||
		len(resp.GetSurfaces()) != 1 || resp.GetHealthIntervalSeconds() != 30 {
		t.Fatalf("InitResponse = %v", resp)
	}
	if s.getHost() == nil || s.getHost().watchdogInterval != time.Minute {
		t.Fatal("host not recorded or watchdog interval not applied")
	}
	_, err = s.Init(ctx, req)
	wantCode(t, err, codes.FailedPrecondition)

	hr, err := s.Health(ctx, &agentv1.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if hr.GetHealth().GetStatus() != agentv1.Health_STATUS_DEGRADED ||
		hr.GetHealth().GetDetails()["k"] != "v" {
		t.Fatalf("Health = %v", hr)
	}

	if _, err := s.Start(ctx, &agentv1.StartRequest{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for m.jobRuns.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.jobRuns.Load() != 1 {
		t.Fatalf("job ran %d times at Start, want 1", m.jobRuns.Load())
	}
	_, err = s.Start(ctx, &agentv1.StartRequest{})
	wantCode(t, err, codes.FailedPrecondition)

	if _, err := s.Stop(ctx, &agentv1.StopRequest{DeadlineSeconds: 5}); err != nil {
		t.Fatal(err)
	}
	if dl, ok := m.stopCtx.Deadline(); !ok || time.Until(dl) > 5*time.Second {
		t.Fatalf("Stop deadline not applied: %v %v", dl, ok)
	}
	// Stop is idempotent, and Shutdown after Stop signals Serve.
	if _, err := s.Stop(ctx, &agentv1.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Shutdown(ctx, &agentv1.ShutdownRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Shutdown(ctx, &agentv1.ShutdownRequest{}); err != nil {
		t.Fatal("second Shutdown blocked or failed:", err)
	}
	select {
	case <-s.shutdown:
	default:
		t.Fatal("Shutdown did not signal Serve")
	}
}

func TestInitRefusals(t *testing.T) {
	ctx := ctxT(t)
	t.Run("identity mismatch", func(t *testing.T) {
		s, _ := newServer(t, &fakeModule{})
		_, err := s.Init(ctx, initReq("other"))
		wantCode(t, err, codes.FailedPrecondition)
	})
	t.Run("protocol mismatch", func(t *testing.T) {
		s, _ := newServer(t, &fakeModule{})
		req := initReq("fake")
		req.Protocol = Protocol + 1
		_, err := s.Init(ctx, req)
		wantCode(t, err, codes.FailedPrecondition)
	})
	t.Run("config rejected", func(t *testing.T) {
		m := &configModule{err: errors.New("bad config")}
		s, _ := newServer(t, m)
		req := initReq("fake")
		req.Config = []byte(`{"a":1}`)
		_, err := s.Init(ctx, req)
		wantCode(t, err, codes.InvalidArgument)
		if string(m.got) != `{"a":1}` {
			t.Fatalf("SetConfig got %q", m.got)
		}
	})
	t.Run("host unreachable", func(t *testing.T) {
		s, _ := newServer(t, &configModule{})
		s.connectHost = func() (*hostClient, error) { return nil, errors.New("no host") }
		_, err := s.Init(ctx, initReq("fake"))
		wantCode(t, err, codes.Unavailable)
	})
	t.Run("module init fails", func(t *testing.T) {
		s, _ := newServer(t, &fakeModule{initErr: errors.New("nope")})
		_, err := s.Init(ctx, initReq("fake"))
		wantCode(t, err, codes.Internal)
		if s.getHost() != nil {
			t.Fatal("host kept after a failed Init")
		}
		// A failed Init leaves the module un-initialised: Start refuses.
		_, err = s.Start(ctx, &agentv1.StartRequest{})
		wantCode(t, err, codes.FailedPrecondition)
	})
}

func TestStartAndStopFailures(t *testing.T) {
	ctx := ctxT(t)
	t.Run("start fails", func(t *testing.T) {
		s, _ := newServer(t, &fakeModule{startErr: errors.New("nope")})
		if _, err := s.Init(ctx, initReq("fake")); err != nil {
			t.Fatal(err)
		}
		_, err := s.Start(ctx, &agentv1.StartRequest{})
		wantCode(t, err, codes.Internal)
	})
	t.Run("stop fails", func(t *testing.T) {
		s, _ := newServer(t, &fakeModule{stopErr: errors.New("stuck")})
		if _, err := s.Init(ctx, initReq("fake")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Start(ctx, &agentv1.StartRequest{}); err != nil {
			t.Fatal(err)
		}
		_, err := s.Stop(ctx, &agentv1.StopRequest{})
		wantCode(t, err, codes.Internal)
		_, err = s.Shutdown(ctx, &agentv1.ShutdownRequest{})
		wantCode(t, err, codes.Internal)
		select {
		case <-s.shutdown:
			t.Fatal("a failed Shutdown still signalled Serve")
		default:
		}
	})
}

// Losing core after Init must reach Serve through hostLost.
func TestInitWatchesForLostCore(t *testing.T) {
	s, h := newServer(t, &fakeModule{})
	s.hostLost = make(chan struct{}, 1)
	ctx := ctxT(t)
	if _, err := s.Init(ctx, initReq("fake")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.getHost().Identity().WhoAmI(ctx); err != nil {
		t.Fatal(err)
	}
	h.srv.Stop()
	select {
	case <-s.hostLost:
	case <-time.After(15 * time.Second):
		t.Fatal("hostLost never fired")
	}
}
