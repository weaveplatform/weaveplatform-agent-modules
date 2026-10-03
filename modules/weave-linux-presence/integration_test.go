//go:build linux

package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk/testkit"
)

// TestModuleUnderStubCore launches the built binary through core's real
// handshake and lifecycle: what the release pipeline ships, not the package.
func TestModuleUnderStubCore(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and launches the module binary")
	}
	bin := filepath.Join(t.TempDir(), wantID)
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(build.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	core := &testkit.StubCore{ModuleID: wantID, Capabilities: []string{"hypervisor.channel"}}
	proc, err := core.Launch(ctx, bin)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer proc.Kill()

	initResp, err := core.Init(ctx, proc)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if r := initResp.GetRequires(); len(r) != 1 || r[0].GetName() != "hypervisor.channel" {
		t.Errorf("requires = %v, want hypervisor.channel", r)
	}
	if _, err := proc.Client.Start(ctx, &agentv1.StartRequest{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	h, err := proc.Client.Health(ctx, &agentv1.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if h.GetHealth().GetStatus() != agentv1.Health_STATUS_HEALTHY {
		t.Errorf("health = %v (%s)", h.GetHealth().GetStatus(), h.GetHealth().GetReason())
	}
	if err := proc.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
