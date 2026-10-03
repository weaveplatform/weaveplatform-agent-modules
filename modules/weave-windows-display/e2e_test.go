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

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavedisplay"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
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
	realDisplays(t)
	client, priv := channel(t, newGDI())
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

// A set end to end, with ChangeDisplaySettingsEx replaced: the request crosses
// the channel, the module picks the mode, and the stand-in records it instead
// of reconfiguring the display of the machine running the test.
func TestSetThroughTheChannel(t *testing.T) {
	f := &fakeUser32{}
	client, priv := channel(t, displays{w: f.win32()})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := weavewire.DisplaySetRequest{Width: 1280, Height: 720}
	if _, err := client.DisplaySet(ctx, req); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth set: err = %v, want ErrNotAuthenticated", err)
	}
	if len(f.changed) != 0 {
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
	if got.ID != `\\.\DISPLAY1` || len(f.changed) != 1 || f.changed[0].DmPelsWidth != 1280 {
		t.Errorf("set reported %+v, changed %+v", got, f.changed)
	}
	_, err = client.DisplaySet(ctx, weavewire.DisplaySetRequest{Scale: 2})
	if !errors.Is(err, weaveclient.ErrUnsupported) {
		t.Errorf("scale: err = %v, want ErrUnsupported", err)
	}
}
