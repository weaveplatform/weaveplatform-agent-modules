//go:build linux

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// The host's view: the real backends behind core's channel gate, reached by
// the host client over real framing. Hello answers before authentication and
// inventory only after it, because that is the order a host meets a machine.
func TestPresenceThroughTheChannel(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	core.Serve(t, newService())
	go core.Run()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := weaveclient.New(ctx, hostConn, weaveclient.Options{Log: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() {
		_ = client.Close()
		_ = core.Close()
	})

	hello, err := client.Hello(ctx)
	if err != nil {
		t.Fatalf("pre-auth hello: %v", err)
	}
	if hello.OS != runtime.GOOS || hello.Kernel == "" || hello.Version != weavewire.ProtocolVersion {
		t.Errorf("hello = %+v", hello)
	}
	if _, err := client.Inventory(ctx); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth inventory: err = %v, want ErrNotAuthenticated", err)
	}

	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	inv, err := client.Inventory(ctx)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if inv.OS != runtime.GOOS || inv.OSVersion == "" || inv.MemoryBytes == 0 || len(inv.Interfaces) == 0 {
		t.Errorf("inventory = %+v", inv)
	}
}
