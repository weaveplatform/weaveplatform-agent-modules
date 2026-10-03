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
	h, err := proc.Client.Health(ctx, &agentv1.HealthRequest{})
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if got := h.GetHealth().GetStatus(); got != agentv1.Health_STATUS_HEALTHY {
		t.Fatalf("health = %v, want healthy", got)
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

// logHost is a Host that only logs: Init touches nothing else, and a nil
// embedded interface makes any other call fail loudly.
type logHost struct{ modulesdk.Host }

func (logHost) Log() *slog.Logger { return slog.New(slog.DiscardHandler) }

// In-process, so the module's own statements count towards coverage; the
// launched binary above does not report any.
func TestFixtureLifecycle(t *testing.T) {
	f := &fixture{}
	if f.ID() != ModuleID || f.Requires() != nil {
		t.Fatalf("id %q, requires %v", f.ID(), f.Requires())
	}
	ctx := context.Background()
	if err := f.Init(ctx, logHost{}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Health().Status; got != modulesdk.HealthHealthy {
		t.Fatalf("health = %v, want healthy", got)
	}
	if err := f.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}
