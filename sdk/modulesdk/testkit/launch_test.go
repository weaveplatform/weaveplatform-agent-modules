package testkit

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
)

func TestLaunchFailures(t *testing.T) {
	for _, c := range []struct {
		mode, want string
		is         error
	}{
		{"exit", "exited before handshake", nil},
		{"hang", "timeout", ErrHandshakeTimeout},
		{"badline", "malformed", handshake.ErrInvalidLine},
		{"wrongproto", "outside the advertised window", ErrProtocolOutOfWindow},
	} {
		t.Run(c.mode, func(t *testing.T) {
			bin := fakeModule(t, c.mode)
			core := &StubCore{ModuleID: "toy", LaunchTimeout: 500 * time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := core.Launch(ctx, bin)
			if err == nil || errors.Is(err, ErrProtocolRefused) ||
				!strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if c.is != nil && !errors.Is(err, c.is) {
				t.Fatalf("err = %v, want errors.Is %v", err, c.is)
			}
		})
	}
}

func TestLaunchHonoursContext(t *testing.T) {
	bin := fakeModule(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	core := &StubCore{ModuleID: "toy", LaunchTimeout: time.Minute}
	if _, err := core.Launch(ctx, bin); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestLaunchMissingBinary(t *testing.T) {
	core := &StubCore{ModuleID: "toy"}
	if _, err := core.Launch(
		context.Background(),
		filepath.Join(t.TempDir(), "absent"),
	); err == nil {
		t.Fatal("launched a binary that does not exist")
	}
}

// WaitExit bounded by a context kills a module that does not exit, and
// Shutdown on a module already torn down reports the RPC failure.
func TestWaitExitAndShutdownErrors(t *testing.T) {
	bin := buildToy(t)
	core := &StubCore{ModuleID: "toy", Data: NewHostData()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proc, err := core.Launch(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer wcancel()
	if err := proc.WaitExit(wctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitExit = %v, want deadline exceeded", err)
	}
	if _, err := proc.Client.Health(ctx, &agentv1.HealthRequest{}); err == nil {
		t.Fatal("module still answering after WaitExit killed it")
	}
	if err := proc.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown of a dead module succeeded")
	}
}
