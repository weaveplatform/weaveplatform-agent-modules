package testkit

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
)

// buildToy builds the toy module once per test binary run.
func buildToy(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "toymodule"+exeSuffix())
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/toymodule")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building toy module: %v\n%s", err, out)
	}
	return bin
}

// exeSuffix: Windows cannot exec a binary without its extension.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func TestHandshakeAndLifecycle(t *testing.T) {
	bin := buildToy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	core := &StubCore{
		ModuleID:     "toy",
		Capabilities: []string{"test.cap"},
		Config:       []byte(`{"speed":"fast"}`),
	}
	proc, err := core.Launch(ctx, bin)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer proc.Kill()

	if proc.Line.Protocol != 1 {
		t.Fatalf("module claimed protocol %d, want 1", proc.Line.Protocol)
	}

	initResp, err := core.Init(ctx, proc)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if len(initResp.GetRequires()) != 1 || initResp.GetRequires()[0].GetName() != "test.cap" {
		t.Errorf("requires = %v, want [test.cap]", initResp.GetRequires())
	}
	if len(initResp.GetSurfaces()) != 1 || initResp.GetSurfaces()[0].GetId() != "toy-card" {
		t.Errorf("surfaces = %v, want [toy-card]", initResp.GetSurfaces())
	}
	// The module wrote to its store during Init, through the host socket.
	if v, ok := proc.Data.StoreGet("init-key"); !ok || string(v) != "init-value" {
		t.Errorf("store init-key = %q,%v — module's host connection not working", v, ok)
	}

	if _, err := proc.Client.Start(ctx, &agentv1.StartRequest{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Start published an event (topic prefixed by module id) and sent a
	// transport message.
	evs := proc.Data.Events()
	if len(evs) != 1 || evs[0].Topic != "toy.started" {
		t.Errorf("events = %v, want [toy.started]", evs)
	}
	sends := proc.Data.Sends()
	if len(sends) != 1 || sends[0].Kind != "heartbeat" {
		t.Errorf("sends = %v, want one heartbeat", sends)
	}

	h, err := proc.Client.Health(ctx, &agentv1.HealthRequest{})
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.GetHealth().GetStatus() != agentv1.Health_STATUS_HEALTHY {
		t.Errorf("health = %v, want healthy", h.GetHealth().GetStatus())
	}

	if _, err := proc.Client.Stop(ctx, &agentv1.StopRequest{DeadlineSeconds: 5}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := proc.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInitEnforcesSequenceAndIdentity(t *testing.T) {
	bin := buildToy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Core claims a different module id than the binary: Init must refuse.
	core := &StubCore{ModuleID: "impostor"}
	proc, err := core.Launch(ctx, bin)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer proc.Kill()
	if _, err := core.Init(ctx, proc); err == nil {
		t.Fatal("Init accepted a module identity mismatch")
	}
	// Start before (successful) Init must refuse.
	if _, err := proc.Client.Start(ctx, &agentv1.StartRequest{}); err == nil {
		t.Fatal("Start accepted before Init")
	}
}

func TestProtocolRefusedCleanly(t *testing.T) {
	bin := buildToy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Core advertising [2,3] must be refused by a protocol-1 module with
	// exit 78 — no listen, no crash loop.
	core := &StubCore{ModuleID: "toy", Window: handshake.Window{Min: 2, Max: 3}}
	_, err := core.Launch(ctx, bin)
	if !errors.Is(err, ErrProtocolRefused) {
		t.Fatalf("launch under window [2,3]: err = %v, want ErrProtocolRefused", err)
	}
}

func TestProtocolAcceptedAtWindowEdge(t *testing.T) {
	bin := buildToy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Window [1,2]: a protocol-1 module is the N-1 case and must be
	// accepted.
	core := &StubCore{ModuleID: "toy", Window: handshake.Window{Min: 1, Max: 2}}
	proc, err := core.Launch(ctx, bin)
	if err != nil {
		t.Fatalf("launch under window [1,2]: %v", err)
	}
	defer proc.Kill()
	if _, err := core.Init(ctx, proc); err != nil {
		t.Fatalf("init: %v", err)
	}
}
