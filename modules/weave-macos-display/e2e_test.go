//go:build darwin

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

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavedisplay"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

func channel(t *testing.T, b weavedisplay.Backend) (*weaveclient.Client, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	core.Serve(t, weavedisplay.NewService(b))
	go core.Run()

	ctx, cancel := context.WithCancel(context.Background())
	client := weaveclient.New(
		ctx,
		hostConn,
		weaveclient.Options{Log: slog.New(slog.DiscardHandler)},
	)
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = core.Close()
	})
	return client, priv
}

// The host's view of the real displays, read through core's channel gate,
// and only after authentication.
func TestRealDisplaysThroughTheChannel(t *testing.T) {
	realDisplay(t)
	client, priv := channel(t, newDisplays())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.DisplayList(ctx); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth list: err = %v, want ErrNotAuthenticated", err)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	got, err := client.DisplayList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Current.Width == 0 {
		t.Errorf("displays = %+v", got)
	}
}

// A set end to end, with CoreGraphics' apply replaced: the request crosses
// the channel, the module picks the mode, and the stand-in records it instead
// of reconfiguring the display of the machine running the test.
func TestSetThroughTheChannel(t *testing.T) {
	f := newFake()
	client, priv := channel(t, displays{q: f.quartz()})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := weavewire.DisplaySetRequest{Width: 2048, Height: 1280}
	if _, err := client.DisplaySet(ctx, req); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth set: err = %v, want ErrNotAuthenticated", err)
	}
	if len(f.applied) != 0 {
		t.Fatal("a set before authentication reached the display")
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	// No display id: the primary display.
	got, err := client.DisplaySet(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "1" || got.Current.Width != 2048 || len(f.applied) != 1 {
		t.Errorf("set reported %+v, applied %+v", got, f.applied)
	}
	if _, err := client.DisplaySet(
		ctx,
		weavewire.DisplaySetRequest{Width: 1, Height: 1},
	); err == nil {
		t.Error("a mode the display does not offer was accepted")
	}
}
