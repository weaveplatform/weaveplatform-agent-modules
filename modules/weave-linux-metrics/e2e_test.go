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

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
)

// The host's view: real samples from this machine behind core's channel gate,
// reached by the host client over real framing, and only after authentication.
func TestMetricsThroughTheChannel(t *testing.T) {
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
	client := weaveclient.New(
		ctx,
		hostConn,
		weaveclient.Options{Log: slog.New(slog.DiscardHandler)},
	)
	t.Cleanup(func() {
		_ = client.Close()
		_ = core.Close()
	})

	if _, err := client.Metrics(ctx); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth metrics: err = %v, want ErrNotAuthenticated", err)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	before := time.Now().Add(-time.Second)
	for range 2 {
		sample, err := client.Metrics(ctx)
		if err != nil {
			t.Fatalf("metrics: %v", err)
		}
		if sample.SampledAt.Before(before) || sample.MemoryTotalBytes == 0 ||
			len(sample.Disks) == 0 {
			t.Errorf("sample = %+v", sample)
		}
		if sample.CPUPercent < 0 || sample.CPUPercent > 100 {
			t.Errorf("CPU = %.2f%%", sample.CPUPercent)
		}
	}
}
