package modulesdk

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
)

// moduleState is the enforced lifecycle sequence. Init exactly once before
// Start; Stop is idempotent; Shutdown ends the process.
type moduleState int

const (
	stateCreated moduleState = iota
	stateInited
	stateStarted
	stateStopped
)

// moduleServer adapts the Module interface onto ModuleService. Core is the
// only client.
type moduleServer struct {
	agentv1.UnimplementedModuleServiceServer

	m    Module
	host *hostClient
	// dialHost is deferred to Init so a module that never gets Init'd
	// never dials.
	connectHost func() (*hostClient, error)
	// shutdown asks Serve to exit; buffered so Shutdown never blocks.
	shutdown chan struct{}
	// hostLost fires when the connection to core's host services drops —
	// the unix orphan-death mechanism for platforms without Pdeathsig
	// (macOS): if core vanishes, the module exits rather than orphaning.
	hostLost chan struct{}
	// healthInterval passed back to core in InitResponse.
	healthInterval uint32

	mu    sync.Mutex
	state moduleState
}

// getHost returns the host client under the lock, so Serve's shutdown path
// doesn't race the Init RPC that sets it.
func (s *moduleServer) getHost() *hostClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.host
}

func (s *moduleServer) Init(
	ctx context.Context,
	req *agentv1.InitRequest,
) (*agentv1.InitResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != stateCreated {
		return nil, status.Errorf(codes.FailedPrecondition, "Init called twice")
	}
	if got, want := req.GetModuleId(), s.m.ID(); got != want {
		// Core thinks it launched a different module than this binary
		// believes it is — a launch/config mismatch (wrong binary staged
		// under this id) or, at worst, a swapped artifact. Refuse either
		// way: the two sides must agree on identity before Init proceeds.
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"module identity mismatch: core says %q, binary is %q",
			got,
			want,
		)
	}
	if req.GetProtocol() != Protocol {
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"protocol mismatch: negotiated %d, built for %d",
			req.GetProtocol(),
			Protocol,
		)
	}

	if cr, ok := s.m.(ConfigReceiver); ok {
		if err := cr.SetConfig(req.GetConfig()); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "module config: %v", err)
		}
	}

	host, err := s.connectHost()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "connecting host services: %v", err)
	}
	s.host = host

	if err := s.m.Init(ctx, host); err != nil {
		host.Close()
		s.host = nil
		return nil, status.Errorf(codes.Internal, "module init: %v", err)
	}
	s.state = stateInited

	// Record the push-watchdog cadence core asked for; the loop starts with
	// the jobs on Start and stops with them on Stop. Zero disables it.
	if secs := req.GetWatchdogIntervalSeconds(); secs > 0 {
		host.setWatchdogInterval(time.Duration(secs) * time.Second)
	}

	// Collect the module's recurring jobs (if any) now; the runtime owns
	// their lifecycle — started at Start, cancelled at Stop.
	if sched, ok := s.m.(Scheduled); ok {
		host.addJobs(sched.Jobs())
	}

	// Watch the host connection: if core dies, exit rather than orphan. The
	// watch outlives this RPC, so it keeps the call's values but not its
	// cancellation.
	if s.hostLost != nil {
		go host.awaitDisconnect(context.WithoutCancel(ctx), func() {
			select {
			case s.hostLost <- struct{}{}:
			default:
			}
		})
	}

	requires := s.m.Requires()
	reqCaps := make([]*agentv1.Capability, 0, len(requires))
	for _, c := range requires {
		reqCaps = append(reqCaps, &agentv1.Capability{Name: string(c)})
	}
	return &agentv1.InitResponse{
		Requires:              reqCaps,
		Surfaces:              host.declaredSurfaces(),
		HealthIntervalSeconds: s.healthInterval,
	}, nil
}

func (s *moduleServer) Start(
	ctx context.Context,
	_ *agentv1.StartRequest,
) (*agentv1.StartResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != stateInited {
		return nil, status.Errorf(codes.FailedPrecondition, "Start in state %d", s.state)
	}
	if err := s.m.Start(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "module start: %v", err)
	}
	// Jobs outlive the RPC: they keep the call's values but not its
	// cancellation, and stopJobs ends them.
	s.host.startJobs(context.WithoutCancel(ctx))
	s.state = stateStarted
	return &agentv1.StartResponse{}, nil
}

func (s *moduleServer) Stop(
	ctx context.Context,
	req *agentv1.StopRequest,
) (*agentv1.StopResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopLocked(ctx, req.GetDeadlineSeconds())
}

func (s *moduleServer) stopLocked(
	ctx context.Context,
	deadlineSeconds uint32,
) (*agentv1.StopResponse, error) {
	switch s.state {
	case stateStopped, stateCreated, stateInited:
		// Idempotent; stopping something never started is a no-op.
		s.state = stateStopped
		return &agentv1.StopResponse{}, nil
	}
	if deadlineSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(deadlineSeconds)*time.Second)
		defer cancel()
	}
	s.host.stopJobs()
	if err := s.m.Stop(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "module stop: %v", err)
	}
	s.state = stateStopped
	return &agentv1.StopResponse{}, nil
}

func (s *moduleServer) Health(
	ctx context.Context,
	_ *agentv1.HealthRequest,
) (*agentv1.HealthResponse, error) {
	h := s.m.Health()
	return &agentv1.HealthResponse{Health: &agentv1.Health{
		Status:  h.Status.wire(),
		Reason:  h.Reason,
		Details: h.Details,
	}}, nil
}

func (s *moduleServer) Shutdown(
	ctx context.Context,
	_ *agentv1.ShutdownRequest,
) (*agentv1.ShutdownResponse, error) {
	s.mu.Lock()
	if _, err := s.stopLocked(ctx, 0); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	select {
	case s.shutdown <- struct{}{}:
	default:
	}
	return &agentv1.ShutdownResponse{}, nil
}
