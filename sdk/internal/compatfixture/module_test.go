package main

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk/testkit"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
)

// buildFixture builds this command, as the compat workflow lays it out.
func buildFixture(t *testing.T, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fixture: %v\n%s", err, b)
	}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// The same lifecycle the released core drives, against the sdk's StubCore:
// if this fails, the compat workflow's failure is the fixture's, not core's.
func TestFixtureUnderStubCore(t *testing.T) {
	bin := filepath.Join(t.TempDir(), ModuleID+exeSuffix())
	buildFixture(t, bin)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	core := &testkit.StubCore{ModuleID: ModuleID}
	proc, err := core.Launch(ctx, bin)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer proc.Kill()

	initResp, err := core.Init(ctx, proc)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if len(initResp.GetRequires()) != 0 {
		t.Errorf("requires = %v, want none", initResp.GetRequires())
	}
	if _, err := proc.Client.Start(ctx, &agentv1.StartRequest{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Healthy once the stub's registry, which lists the launched module,
	// has been read through the host connection.
	var h *agentv1.HealthResponse
	for {
		if h, err = proc.Client.Health(ctx, &agentv1.HealthRequest{}); err != nil {
			t.Fatalf("health: %v", err)
		}
		if h.GetHealth().GetStatus() == agentv1.Health_STATUS_HEALTHY || ctx.Err() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := h.GetHealth().GetStatus(); got != agentv1.Health_STATUS_HEALTHY {
		t.Fatalf("health = %v (%s), want healthy", got, h.GetHealth().GetReason())
	}
	if _, err := proc.Client.Stop(ctx, &agentv1.StopRequest{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Shutdown waits for the process: a clean exit is the last thing core
	// sees of a module it stops.
	if err := proc.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// logHost is a Host that logs and has a registry: the fixture touches
// nothing else, and a nil embedded interface makes any other call fail
// loudly.
type logHost struct {
	modulesdk.Host
	reg *weavemoduletest.Registry
}

func (logHost) Log() *slog.Logger { return slog.New(slog.DiscardHandler) }

func (h logHost) Registry() modulesdk.Registry { return h.reg }

// In-process, so the module's own statements count towards coverage; the
// launched binary above does not report any.
func TestFixtureLifecycle(t *testing.T) {
	f := &fixture{}
	if f.ID() != ModuleID || f.Requires() != nil {
		t.Fatalf("id %q, requires %v", f.ID(), f.Requires())
	}
	ctx := context.Background()
	reg := weavemoduletest.NewRegistry()
	if err := f.Init(ctx, logHost{reg: reg}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// Not listed yet: degraded, saying why.
	waitHealth(t, f, modulesdk.HealthDegraded, "registry: not listed")
	reg.SetModule(modulesdk.RegisteredModule{ID: ModuleID, Address: ModuleID, State: "running"})
	waitHealth(t, f, modulesdk.HealthHealthy, "")
	if err := f.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	// Under a core with no registry the fixture says so rather than
	// claiming health it has not checked.
	g := &fixture{}
	reg = weavemoduletest.NewRegistry()
	reg.Fail(modulesdk.ErrRegistryUnsupported)
	_ = g.Init(ctx, logHost{reg: reg})
	_ = g.Start(ctx)
	waitHealth(
		t,
		g,
		modulesdk.HealthDegraded,
		"registry: "+modulesdk.ErrRegistryUnsupported.Error(),
	)
	_ = g.Stop(ctx)
	// Stop before Start has nothing to stop.
	if err := (&fixture{}).Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func waitHealth(t *testing.T, f *fixture, status modulesdk.HealthStatus, reason string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h := f.Health()
		if h.Status == status && h.Reason == reason {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health = %+v, want %v %q", h, status, reason)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// moduleRow reads weavectl's table by its header, before and after core
// added the ADDRESS column.
func TestModuleRow(t *testing.T) {
	const before = "MODULE VERSION PROTO STATE PID RESTARTS HEALTH\n" +
		"weave-compat-fixture 0.1.0 1 running 9 0 STATUS_DEGRADED (registry: not listed)\n"
	const after = "MODULE ADDRESS VERSION PROTO STATE PID RESTARTS HEALTH\n" +
		"other x 0.1.0 1 stopped - 0 STATUS_UNSPECIFIED\n" +
		"weave-compat-fixture weave-compat-fixture 0.1.0 1 running 9 0 STATUS_HEALTHY\n"
	if state, health, ok := moduleRow(before, ModuleID); !ok || state != "running" ||
		health != "STATUS_DEGRADED (registry: not listed)" {
		t.Fatalf("before: %q %q %v", state, health, ok)
	}
	if state, health, ok := moduleRow(
		after,
		ModuleID,
	); !ok || state != "running" ||
		health != "STATUS_HEALTHY" {
		t.Fatalf("after: %q %q %v", state, health, ok)
	}
	if _, _, ok := moduleRow(
		"weave-compat-fixture 0.1.0 1 running 9 0 STATUS_HEALTHY\n",
		ModuleID,
	); ok {
		t.Fatal("a row with no header was read")
	}
	if _, _, ok := moduleRow(after, "absent"); ok {
		t.Fatal("found a module that is not there")
	}
}
