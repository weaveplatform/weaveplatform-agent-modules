//go:build windows

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavesession"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// standIn is a console the test controls, so a login can happen on cue.
type standIn struct {
	mu  sync.Mutex
	cur *weavewire.SessionInfo
}

func (s *standIn) Console(context.Context) (weavewire.SessionInfo, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return weavewire.SessionInfo{}, false, nil
	}
	return *s.cur, true, nil
}

func (s *standIn) List(ctx context.Context) ([]weavewire.SessionInfo, error) {
	cur, ok, _ := s.Console(ctx)
	if !ok {
		return nil, nil
	}
	return []weavewire.SessionInfo{cur}, nil
}

func (s *standIn) set(info *weavewire.SessionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = info
}

func channel(t *testing.T, svc *weavesession.Service) (*weaveclient.Client, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	core.Serve(t, svc)
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

// The host's view through core's channel gate: current and list answer only
// after authentication, and a login, a switch and a logout at the console
// arrive as change events.
func TestSessionThroughTheChannel(t *testing.T) {
	alice := &weavewire.SessionInfo{
		ID:    "1",
		User:  `WS01\alice`,
		UID:   "S-1-5-21-1-2-3-1001",
		State: weavewire.SessionActive,
	}
	b := &standIn{cur: alice}
	client, priv := channel(
		t,
		weavesession.NewService(b, weavesession.WithPollInterval(10*time.Millisecond)),
	)
	changed := make(chan weavewire.SessionChangedEvent, 4)
	client.OnSessionChanged(func(ev weavewire.SessionChangedEvent) { changed <- ev })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.SessionCurrent(ctx); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth current: err = %v, want ErrNotAuthenticated", err)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	cur, err := client.SessionCurrent(ctx)
	if err != nil || cur == nil || cur.User != `WS01\alice` || !cur.Console {
		t.Fatalf("current = %+v, %v", cur, err)
	}
	list, err := client.SessionList(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "1" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	locked := *alice
	locked.State = weavewire.SessionLocked
	for _, next := range []*weavewire.SessionInfo{
		&locked,
		{ID: "2", User: `WS01\bob`, UID: "S-1-5-21-1-2-3-1002", State: weavewire.SessionActive},
		nil,
	} {
		b.set(next)
		select {
		case ev := <-changed:
			if (ev.Current == nil) != (next == nil) ||
				(next != nil && (ev.Current.ID != next.ID || ev.Current.State != next.State)) {
				t.Errorf("change to %+v, want %+v", ev.Current, next)
			}
		case <-ctx.Done():
			t.Fatalf("no change event for %+v", next)
		}
	}
}

// The real backend through the channel: whatever this machine's Terminal Services report,
// as an answer rather than an error, and lock refused as
// unsupported.
func TestRealSessionThroughTheChannel(t *testing.T) {
	client, priv := channel(t, newService())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	cur, err := client.SessionCurrent(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	t.Logf("console: %+v", cur)
	if _, err := client.SessionList(ctx); err != nil {
		t.Fatalf("list: %v", err)
	}
	// Windows locks only the caller's own session, so the host gets an
	// answer it can feature-gate on rather than silence.
	if _, err := client.SessionLock(ctx, ""); !errors.Is(err, weaveclient.ErrUnsupported) {
		t.Errorf("lock: err = %v, want ErrUnsupported", err)
	}
}
