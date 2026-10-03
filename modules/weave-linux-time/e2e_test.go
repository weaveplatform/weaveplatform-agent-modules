//go:build linux

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
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavetime"
)

// recordingClock stands in for the module's clock, so a test never moves the
// clock of the machine running it.
type recordingClock struct{ set chan time.Time }

func (c recordingClock) SetTime(_ context.Context, t time.Time) error {
	c.set <- t
	return nil
}

func channel(t *testing.T, b weavetime.Backend) (*weaveclient.Client, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	core.Serve(t, weavetime.NewService(b))
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

// The host's view: the real clock read through core's channel gate, and a
// correction delivered to the backend, both only after authentication.
func TestTimeThroughTheChannel(t *testing.T) {
	clk := recordingClock{set: make(chan time.Time, 1)}
	client, priv := channel(t, clk)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.Time(ctx); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth time: err = %v, want ErrNotAuthenticated", err)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	got, err := client.Time(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if drift := time.Since(time.Unix(0, got.UnixNano)); drift < 0 || drift > 10*time.Second {
		t.Errorf("guest clock reads %v, %v from this one", time.Unix(0, got.UnixNano), drift)
	}

	target := time.Now().Add(90 * time.Second).Truncate(time.Millisecond)
	resp, err := client.SetTime(ctx, target, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if set := <-clk.set; !set.Equal(target) {
		t.Errorf("backend set %v, want %v", set, target)
	}
	if skew := resp.Skew(); skew < 80*time.Second || skew > 100*time.Second {
		t.Errorf("reported skew %v, want about 90s", skew)
	}

	// A correction larger than the host allowed never reaches the backend.
	if _, err := client.SetTime(ctx, time.Now().Add(48*time.Hour), time.Minute); err == nil {
		t.Fatal("a correction over the host's own limit was applied")
	}
	select {
	case set := <-clk.set:
		t.Errorf("the refused correction reached the clock: %v", set)
	default:
	}
}
