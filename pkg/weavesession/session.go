// Package weavesession is the session capability: who is logged in at the
// console and elsewhere, lock a session, and an event whenever the console
// session changes.
//
// It runs as system, so it answers with nobody logged in — which is the state
// in which the per-user-console capabilities (clipboard, display) do not
// answer at all, and the reason a host asks it.
package weavesession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveagent"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Backend reports the session at the physical console. Every OS can answer
// this, so it is the one method a backend must have.
type Backend interface {
	// Console reports the console session, and false when nobody is logged
	// in there (a login window, a greeter, session 0).
	Console(ctx context.Context) (weavewire.SessionInfo, bool, error)
}

// Lister is implemented by a backend that can enumerate every logged-in
// session. Without it, list answers weavewire.CodeUnsupported.
type Lister interface {
	List(ctx context.Context) ([]weavewire.SessionInfo, error)
}

// Locker is implemented by a backend that can lock a session's screen.
// Without it, lock answers weavewire.CodeUnsupported.
type Locker interface {
	Lock(ctx context.Context, sessionID string) error
}

// ErrNoConsoleSession reports a lock of "the console session" with nobody
// logged in there.
var ErrNoConsoleSession = errors.New("weavesession: nobody is logged in at the console")

// DefaultPollInterval is how often the console session is checked for a
// change to report. It matches agent-core's own console watcher: a console
// switch is a human-speed event, and each OS's change notification needs
// different plumbing for no gain at that speed.
const DefaultPollInterval = 2 * time.Second

// Option configures a Service.
type Option func(*Service)

// WithPollInterval sets how often the console session is checked for a change.
func WithPollInterval(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.interval = d
		}
	}
}

// Service is the session capability over an OS backend.
type Service struct {
	b        Backend
	interval time.Duration

	emit weaveagent.Emitter
	log  *slog.Logger
}

// NewService builds the session service.
func NewService(b Backend, opts ...Option) *Service {
	s := &Service{b: b, interval: DefaultPollInterval}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Capability implements weavemodule.Service.
func (s *Service) Capability() weavewire.Capability { return weavewire.Session }

// Register implements weavemodule.Service. An op the backend cannot do is
// registered as unsupported, so every OS serves the same kinds and the host
// feature-gates on the answer.
func (s *Service) Register(r *weavemodule.Registrar) error {
	s.emit = r.Emitter()
	s.log = r.Log()
	r.Handle(weavewire.KindSessionCurrent, s.handleCurrent)
	if l, ok := s.b.(Lister); ok {
		r.Handle(weavewire.KindSessionList, func(ctx context.Context, _ []byte) ([]byte, error) {
			sessions, err := l.List(ctx)
			if err != nil {
				return nil, fmt.Errorf("weavesession: listing: %w", err)
			}
			if sessions == nil {
				sessions = []weavewire.SessionInfo{}
			}
			return weavewire.EncodePayload(weavewire.SessionListResponse{Sessions: sessions})
		})
	} else {
		r.Unsupported(weavewire.KindSessionList, "this OS cannot enumerate its sessions")
	}
	if l, ok := s.b.(Locker); ok {
		r.Handle(
			weavewire.KindSessionLock,
			func(ctx context.Context, payload []byte) ([]byte, error) {
				return s.handleLock(ctx, l, payload)
			},
		)
	} else {
		r.Unsupported(weavewire.KindSessionLock, "this OS cannot lock a session from a service")
	}
	return nil
}

func (s *Service) handleCurrent(ctx context.Context, _ []byte) ([]byte, error) {
	cur, err := s.console(ctx)
	if err != nil {
		return nil, err
	}
	return weavewire.EncodePayload(weavewire.SessionCurrentResponse{Session: cur})
}

func (s *Service) console(ctx context.Context) (*weavewire.SessionInfo, error) {
	info, ok, err := s.b.Console(ctx)
	if err != nil {
		return nil, fmt.Errorf("weavesession: reading the console session: %w", err)
	}
	if !ok {
		return nil, nil //nolint:nilnil // nobody at the console is an answer, not a failure
	}
	info.Console = true
	return &info, nil
}

func (s *Service) handleLock(ctx context.Context, l Locker, payload []byte) ([]byte, error) {
	var req weavewire.SessionLockRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("weavesession: decoding lock: %w", err)
		}
	}
	id := req.SessionID
	if id == "" {
		cur, err := s.console(ctx)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, ErrNoConsoleSession
		}
		id = cur.ID
	}
	if err := l.Lock(ctx, id); err != nil {
		return nil, fmt.Errorf("weavesession: locking %s: %w", id, err)
	}
	return weavewire.EncodePayload(weavewire.SessionLockResponse{SessionID: id})
}

// Start implements weavemodule.Starter: it watches the console session and
// emits weavewire.KindSessionChanged whenever it changes, until ctx ends.
func (s *Service) Start(ctx context.Context) error {
	go s.watch(ctx)
	return nil
}

func (s *Service) watch(ctx context.Context) {
	// The first good reading is the baseline, not a change: a module that
	// starts (or restarts) has nothing new to tell a host that can ask
	// current.
	var (
		last  *weavewire.SessionInfo
		known bool
	)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		cur, err := s.console(ctx)
		switch {
		case err != nil:
			// A failed probe is not "nobody is logged in": reporting a
			// logout that did not happen would have the host tear down
			// its clipboard sync. Keep the last known state.
			s.log.Debug("weave: reading the console session", "err", err)
		case !known:
			last, known = cur, true
		case !same(last, cur):
			ev := weavewire.SessionChangedEvent{
				Previous:  last,
				Current:   cur,
				ChangedAt: time.Now().UTC(),
			}
			if err := s.emit.Emit(ctx, weavewire.KindSessionChanged, ev); err != nil {
				// No host connected is ordinary; it asks current when it is.
				s.log.Debug("weave: emitting a session change", "err", err)
			}
			last = cur
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// same reports whether two readings are the same session in the same state.
// Since is left out: some OSes derive it, and a jitter in it is not a change.
func same(a, b *weavewire.SessionInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.User == b.User && a.UID == b.UID && a.State == b.State
}
