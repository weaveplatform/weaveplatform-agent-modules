//go:build windows

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/shutdown"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavepower"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// The host's view: a power request is acknowledged with the call the machine
// is about to make, and only then is it made. The module's backend decides;
// only InitiateShutdown is a stand-in. Power is never open before
// authentication.
func TestPowerThroughTheChannel(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	ran := make(chan shutdown.SHUTDOWN_FLAGS, 1)
	backend := power{
		enable:   func(string) error { return nil },
		initiate: func(_ string, flags shutdown.SHUTDOWN_FLAGS) uint32 { ran <- flags; return 0 },
	}
	core.Serve(t, weavepower.NewService(backend))
	go core.Run()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := weaveclient.New(
		ctx,
		hostConn,
		weaveclient.Options{Log: slog.New(slog.DiscardHandler)},
	)
	t.Cleanup(func() {
		_ = client.Close()
		_ = core.Close()
	})

	if _, err := client.Shutdown(ctx, "test"); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth shutdown: err = %v, want ErrNotAuthenticated", err)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	for kind, mode := range map[string]shutdown.SHUTDOWN_FLAGS{
		weavewire.KindPowerShutdown: shutdown.SHUTDOWN_POWEROFF,
		weavewire.KindPowerRestart:  shutdown.SHUTDOWN_RESTART,
	} {
		var resp weavewire.PowerResponse
		if err := client.Call(
			ctx,
			kind,
			weavewire.PowerRequest{Reason: "test"},
			&resp,
		); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !resp.Accepted || resp.Command == "" {
			t.Errorf("%s = %+v", kind, resp)
		}
		select {
		case flags := <-ran:
			if flags != shutdownFlags|mode {
				t.Errorf("%s initiated with %#x", kind, uint32(flags))
			}
		case <-ctx.Done():
			t.Fatalf("%s was acknowledged and never run", kind)
		}
	}
}
