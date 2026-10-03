//go:build linux

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavepower"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// wantCommands are the commands the real backend may decide on: systemd's own
// where systemctl is on PATH, shutdown(8) where it is not.
var wantCommands = map[string][]string{
	weavewire.KindPowerShutdown: {"systemctl poweroff", "shutdown -h now"},
	weavewire.KindPowerRestart:  {"systemctl reboot", "shutdown -r now"},
}

// dryRun is the module's real backend with only the final step replaced: it
// decides exactly as the module does, and records the run instead of powering
// off the machine running the test.
type dryRun struct {
	real weavepower.Backend
	ran  chan string
}

func (d dryRun) Shutdown(ctx context.Context, reason string) (weavepower.Action, error) {
	return d.stub(d.real.Shutdown(ctx, reason))
}

func (d dryRun) Restart(ctx context.Context, reason string) (weavepower.Action, error) {
	return d.stub(d.real.Restart(ctx, reason))
}

func (d dryRun) stub(a weavepower.Action, err error) (weavepower.Action, error) {
	a.Run = func() error { d.ran <- a.Command; return nil }
	return a, err
}

// The host's view: a power request is acknowledged with the command the guest
// is about to run, and only then is it run. Power is never open before
// authentication.
func TestPowerThroughTheChannel(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	ran := make(chan string, 1)
	core.Serve(t, weavepower.NewService(dryRun{real: weavepower.Unix{}, ran: ran}))
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

	for kind, want := range wantCommands {
		var resp weavewire.PowerResponse
		if err := client.Call(
			ctx,
			kind,
			weavewire.PowerRequest{Reason: "test"},
			&resp,
		); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !resp.Accepted || !slices.Contains(want, resp.Command) {
			t.Errorf("%s = %+v, want accepted with one of %q", kind, resp, want)
		}
		select {
		case got := <-ran:
			if got != resp.Command {
				t.Errorf("%s ran %q after acknowledging %q", kind, got, resp.Command)
			}
		case <-ctx.Done():
			t.Fatalf("%s was acknowledged and never run", kind)
		}
	}
}
