package weavemodule_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveagent"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// echoService serves the time capability with handlers that echo, so the
// runtime can be tested without any real capability logic.
type echoService struct {
	capability weavewire.Capability
	register   func(r *weavemodule.Registrar) error
	stopped    bool
	stopErr    error
	health     *modulesdk.Health
}

func (s *echoService) Capability() weavewire.Capability { return s.capability }

func (s *echoService) Register(r *weavemodule.Registrar) error { return s.register(r) }

type stoppingService struct{ *echoService }

func (s stoppingService) Stop(context.Context) error {
	s.stopped = true
	return s.stopErr
}

type healthService struct{ *echoService }

func (s healthService) Health() modulesdk.Health { return *s.health }

func echo(_ context.Context, payload []byte) ([]byte, error) { return payload, nil }

func timeService() *echoService {
	return &echoService{capability: weavewire.Time, register: func(r *weavemodule.Registrar) error {
		r.Handle(weavewire.KindTimeGet, echo)
		r.HandleDeferred(
			weavewire.KindTimeSet,
			func(ctx context.Context, p []byte) ([]byte, func(), error) {
				out, err := echo(ctx, p)
				return out, nil, err
			},
		)
		if r.Host() == nil || r.Log() == nil || r.Emitter() == nil {
			return errors.New("registrar is missing a surface")
		}
		return nil
	}}
}

func TestModuleIdentity(t *testing.T) {
	m := weavemodule.New(
		weavemodule.ModuleID("linux", weavewire.Exec),
		&echoService{capability: weavewire.Exec},
	)
	if m.ID() != "weave-linux-exec" {
		t.Fatalf("id = %q", m.ID())
	}
	// The address is the capability's, not the module id: every OS variant
	// shares it, which is what lets the host ignore the guest OS.
	if m.Address() != "weave.exec" {
		t.Fatalf("address = %q", m.Address())
	}
	if !slices.Equal(m.Requires(), []modulesdk.Capability{"hypervisor.channel"}) {
		t.Fatalf("requires = %v", m.Requires())
	}
}

func TestModuleRoundTripsThroughItsService(t *testing.T) {
	h := weavemoduletest.Start(t, timeService())
	var got map[string]string
	h.Decode(weavewire.KindTimeGet, map[string]string{"hi": "there"}, &got)
	if got["hi"] != "there" {
		t.Fatalf("payload lost: %v", got)
	}
	h.Decode(weavewire.KindTimeSet, map[string]string{"a": "b"}, &got)
	if got["a"] != "b" {
		t.Fatalf("deferred payload lost: %v", got)
	}
	if want := weavewire.Time.Ops(); !slices.Equal(h.Module.Kinds(), want) {
		t.Fatalf("kinds = %v, want %v", h.Module.Kinds(), want)
	}
	if h.Module.Health().Status != modulesdk.HealthHealthy {
		t.Fatal("a service with no health report is not healthy")
	}
}

// A handler registered for another capability's kind could never be reached,
// because core routes by address. Init must say so instead of serving it.
func TestInitRefusesKindsOutsideTheCapability(t *testing.T) {
	cases := map[string]func(r *weavemodule.Registrar){
		"another capability": func(r *weavemodule.Registrar) { r.Handle(weavewire.KindExecStart, echo) },
		"not a weave kind": func(r *weavemodule.Registrar) {
			r.Handle("sysinfo.collect", echo)
		},
		"a reply kind": func(r *weavemodule.Registrar) {
			r.Handle(weavewire.ResultKind(weavewire.KindTimeGet), echo)
		},
		"a duplicate": func(r *weavemodule.Registrar) {
			r.Handle(weavewire.KindTimeGet, echo)
			r.Handle(weavewire.KindTimeGet, echo)
		},
		"deferred, wrong capability": func(r *weavemodule.Registrar) {
			r.HandleDeferred(weavewire.KindPowerShutdown, nil)
		},
	}
	for name, register := range cases {
		t.Run(name, func(t *testing.T) {
			svc := &echoService{
				capability: weavewire.Time,
				register: func(r *weavemodule.Registrar) error {
					register(r)
					return nil
				},
			}
			m := weavemodule.New("weave-test-time", svc)
			err := m.Init(
				context.Background(),
				weavemoduletest.NewHost(weavemoduletest.NewTransport()),
			)
			if err == nil {
				t.Fatal("Init accepted a kind the module could never be addressed with")
			}
		})
	}
}

func TestInitReportsTheServicesOwnError(t *testing.T) {
	svc := &echoService{capability: weavewire.Time, register: func(*weavemodule.Registrar) error {
		return errors.New("no clock device")
	}}
	err := weavemodule.New("weave-test-time", svc).Init(context.Background(),
		weavemoduletest.NewHost(weavemoduletest.NewTransport()))
	if err == nil || !strings.Contains(err.Error(), "no clock device") {
		t.Fatalf("err = %v", err)
	}
}

// An op the OS cannot do answers with a code a host can branch on.
func TestUnsupportedOpsAnswerWithTheCode(t *testing.T) {
	svc := &echoService{capability: weavewire.Time, register: func(r *weavemodule.Registrar) error {
		r.Handle(weavewire.KindTimeGet, echo)
		r.Unsupported(weavewire.KindTimeSet, "the clock is owned by the hypervisor")
		return nil
	}}
	h := weavemoduletest.Start(t, svc)
	res := h.Call(weavewire.KindTimeSet, nil)
	if res.Code != weavewire.CodeUnsupported {
		t.Fatalf("code = %q, want %q (err %q)", res.Code, weavewire.CodeUnsupported, res.Err)
	}
	if !strings.Contains(res.Err, "owned by the hypervisor") {
		t.Fatalf("reason lost: %q", res.Err)
	}
	// Unsupported still counts as served: the op is answered, honestly.
	if err := weavemodule.CheckParity(svc); err != nil {
		t.Fatal(err)
	}
}

func TestCheckParity(t *testing.T) {
	if err := weavemodule.CheckParity(timeService()); err != nil {
		t.Fatalf("a complete service failed parity: %v", err)
	}
	partial := &echoService{
		capability: weavewire.Time,
		register: func(r *weavemodule.Registrar) error {
			r.Handle(weavewire.KindTimeGet, echo)
			return nil
		},
	}
	if err := weavemodule.CheckParity(partial); err == nil {
		t.Fatal("a service missing an op passed parity")
	}
	broken := &echoService{
		capability: weavewire.Time,
		register: func(*weavemodule.Registrar) error {
			return errors.New("boom")
		},
	}
	if err := weavemodule.CheckParity(broken); err == nil {
		t.Fatal("a service that failed to register passed parity")
	}
}

func TestStopReachesTheService(t *testing.T) {
	svc := stoppingService{timeService()}
	m := weavemodule.New("weave-test-time", svc)
	// Stop before Start must be safe: core may stop a module whose Start failed.
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.stopErr = errors.New("processes would not die")
	if err := m.Stop(context.Background()); err == nil {
		t.Fatal("a service's stop failure was swallowed")
	}
}

func TestHealthComesFromTheService(t *testing.T) {
	degraded := modulesdk.Health{Status: modulesdk.HealthDegraded, Reason: "no console session"}
	svc := healthService{timeService()}
	svc.health = &degraded
	m := weavemodule.New("weave-test-time", svc)
	if got := m.Health(); got.Status != modulesdk.HealthDegraded {
		t.Fatalf("health = %+v", got)
	}
}

type refusingTransport struct{ weavemoduletest.Transport }

func (*refusingTransport) Receive(context.Context) (<-chan modulesdk.Message, error) {
	return nil, errors.New("core went away")
}

type refusingHost struct{ *weavemoduletest.Host }

func (h refusingHost) Transport() modulesdk.Transport { return &refusingTransport{} }

func TestStartReportsAReceiveFailure(t *testing.T) {
	m := weavemodule.New("weave-test-time", timeService())
	host := refusingHost{weavemoduletest.NewHost(weavemoduletest.NewTransport())}
	if err := m.Init(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded with no inbound stream")
	}
}

type nilLogHost struct{ *weavemoduletest.Host }

func (nilLogHost) Log() *slog.Logger { return nil }

func TestInitToleratesAHostWithoutALogger(t *testing.T) {
	m := weavemodule.New("weave-test-time", timeService())
	if err := m.Init(
		context.Background(),
		nilLogHost{weavemoduletest.NewHost(weavemoduletest.NewTransport())},
	); err != nil {
		t.Fatal(err)
	}
}

// Events a service emits go out on the module's transport, unaddressed by
// correlation id, as the host's event routing expects.
func TestRegistrarEmitterSendsEvents(t *testing.T) {
	var emit weaveagent.Emitter
	svc := &echoService{capability: weavewire.Time, register: func(r *weavemodule.Registrar) error {
		emit = r.Emitter()
		r.Handle(weavewire.KindTimeGet, echo)
		r.Handle(weavewire.KindTimeSet, echo)
		return nil
	}}
	h := weavemoduletest.Start(t, svc)
	if err := emit.Emit(
		context.Background(),
		"weave.time.drift",
		map[string]int{"s": 3},
	); err != nil {
		t.Fatal(err)
	}
	sent := h.T.Sent()
	if len(sent) != 1 || sent[0].Kind != "weave.time.drift" {
		t.Fatalf("sent = %+v", sent)
	}
	var body map[string]int
	if err := json.Unmarshal(sent[0].Data, &body); err != nil || body["s"] != 3 {
		t.Fatalf("event body = %s (%v)", sent[0].Data, err)
	}
}

type startingService struct {
	*echoService
	started chan context.Context
	err     error
}

func (s startingService) Start(ctx context.Context) error {
	if s.err != nil {
		return s.err
	}
	s.started <- ctx
	return nil
}

// A service's own work starts with the module and ends when it stops.
func TestStartRunsTheServicesOwnWorkUntilStop(t *testing.T) {
	svc := startingService{echoService: timeService(), started: make(chan context.Context, 1)}
	h := weavemoduletest.Start(t, svc)
	ctx := <-svc.started
	if ctx.Err() != nil {
		t.Fatal("the service's context ended at start")
	}
	if err := h.Module.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the service's context outlived the module")
	}
}

func TestStartReportsAServiceThatCannotStart(t *testing.T) {
	svc := startingService{echoService: timeService(), err: errors.New("no watcher")}
	m := weavemodule.New("weave-test-time", svc)
	if err := m.Init(context.Background(), weavemoduletest.NewHost(weavemoduletest.NewTransport())); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no watcher") {
		t.Fatalf("err = %v", err)
	}
}
