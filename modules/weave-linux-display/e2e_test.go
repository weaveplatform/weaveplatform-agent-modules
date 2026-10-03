//go:build linux

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

func channel(t *testing.T) (*weaveclient.Client, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	core.Serve(t, newService())
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

// The host's view of an X11 session, with fake xrandr and cvt standing in for
// the real ones: listing and a set cross core's channel gate only after
// authentication, and the set reaches the tool as the arguments it would run.
func TestDisplayThroughTheChannel(t *testing.T) {
	calls := session(t, map[string]string{"DISPLAY": ":0"}, "xrandr", "cvt")
	client, priv := channel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.DisplayList(ctx); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth list: err = %v, want ErrNotAuthenticated", err)
	}
	if _, err := client.DisplaySet(
		ctx,
		weavewire.DisplaySetRequest{Width: 1024, Height: 768},
	); !errors.Is(
		err,
		weaveclient.ErrNotAuthenticated,
	) {
		t.Fatalf("pre-auth set: err = %v, want ErrNotAuthenticated", err)
	}
	if got := calls(); len(got) != 1 || got[0] != "" {
		t.Fatalf("a refused request reached the tools: %q", got)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	got, err := client.DisplayList(ctx)
	if err != nil || len(got) != 3 {
		t.Fatalf("list = %+v, %v", got, err)
	}
	// No display id: the primary output.
	if _, err := client.DisplaySet(
		ctx,
		weavewire.DisplaySetRequest{Width: 1024, Height: 768},
	); err != nil {
		t.Fatal(err)
	}
	if c := calls(); !strings.Contains(
		strings.Join(c, "|"),
		"xrandr --output Virtual-1 --mode 1024x768",
	) {
		t.Errorf("calls %q", c)
	}
}

// A session with no display server — a CI runner, a console login — answers
// unsupported, which the host can feature-gate on.
func TestNoDisplayServerThroughTheChannel(t *testing.T) {
	session(t, nil)
	client, priv := channel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, err := client.DisplayList(ctx); !errors.Is(err, weaveclient.ErrUnsupported) {
		t.Errorf("list: err = %v, want ErrUnsupported", err)
	}
}
