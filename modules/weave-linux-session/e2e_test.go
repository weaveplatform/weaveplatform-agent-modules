//go:build linux

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

// standIn is a console the test controls, so a lock never reaches the session
// of the machine running it and a login can happen on cue.
type standIn struct {
	mu     sync.Mutex
	cur    *weavewire.SessionInfo
	locked chan string
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

func (s *standIn) Lock(_ context.Context, id string) error {
	s.locked <- id
	return nil
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

// The host's view through core's channel gate: current, list and lock answer
// only after authentication, a lock of "the console" resolves to its session,
// and a login at the console arrives as a change event.
func TestSessionThroughTheChannel(t *testing.T) {
	alice := &weavewire.SessionInfo{
		ID:    "2",
		User:  "alice",
		UID:   "1000",
		State: weavewire.SessionActive,
	}
	b := &standIn{cur: alice, locked: make(chan string, 2)}
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
	if _, err := client.SessionLock(ctx, ""); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth lock: err = %v, want ErrNotAuthenticated", err)
	}
	select {
	case id := <-b.locked:
		t.Fatalf("an unauthenticated lock reached the backend: %s", id)
	default:
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	cur, err := client.SessionCurrent(ctx)
	if err != nil || cur == nil || cur.User != "alice" || !cur.Console {
		t.Fatalf("current = %+v, %v", cur, err)
	}
	list, err := client.SessionList(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "2" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if id, err := client.SessionLock(ctx, ""); err != nil || id != "2" || <-b.locked != "2" {
		t.Fatalf("lock of the console = %q, %v", id, err)
	}
	if id, err := client.SessionLock(ctx, "c4"); err != nil || id != "c4" || <-b.locked != "c4" {
		t.Fatalf("lock of c4 = %q, %v", id, err)
	}

	bob := &weavewire.SessionInfo{ID: "3", User: "bob", UID: "1001", State: weavewire.SessionActive}
	b.set(bob)
	select {
	case ev := <-changed:
		if ev.Previous == nil || ev.Previous.ID != "2" || ev.Current == nil ||
			ev.Current.ID != "3" {
			t.Errorf("change = %+v -> %+v, want alice's session -> bob's", ev.Previous, ev.Current)
		}
	case <-ctx.Done():
		t.Fatal("no change event for a new console session")
	}

	b.set(nil)
	select {
	case ev := <-changed:
		if ev.Current != nil {
			t.Errorf("logout reported as %+v", ev.Current)
		}
	case <-ctx.Done():
		t.Fatal("no change event for a logout")
	}
	if id, err := client.SessionLock(ctx, ""); err == nil {
		t.Errorf("locked %q with nobody at the console", id)
	}
}

// The real backend through the channel: whatever this machine's logind says,
// as an answer rather than an error.
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
}
