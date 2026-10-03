package weavesession_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavesession"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// console is a backend whose console session the test changes.
type console struct {
	mu    sync.Mutex
	cur   *weavewire.SessionInfo
	err   error
	calls int
}

// settle waits until the watcher has read the console twice more, so the
// state set before it has certainly been seen.
func (c *console) settle(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	target := c.calls + 2
	c.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := c.calls
		c.mu.Unlock()
		if n >= target {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the watcher stopped reading the console")
}

func (c *console) set(cur *weavewire.SessionInfo, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur, c.err = cur, err
}

func (c *console) Console(context.Context) (weavewire.SessionInfo, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return weavewire.SessionInfo{}, false, c.err
	}
	if c.cur == nil {
		return weavewire.SessionInfo{}, false, nil
	}
	return *c.cur, true, nil
}

// full adds list and lock.
type full struct {
	*console
	sessions []weavewire.SessionInfo
	listErr  error
	locked   []string
	lockErr  error
}

func (f *full) List(context.Context) ([]weavewire.SessionInfo, error) { return f.sessions, f.listErr }

func (f *full) Lock(_ context.Context, id string) error {
	if f.lockErr != nil {
		return f.lockErr
	}
	f.locked = append(f.locked, id)
	return nil
}

func alice(state weavewire.SessionState) *weavewire.SessionInfo {
	return &weavewire.SessionInfo{ID: "2", User: "alice", UID: "501", State: state}
}

func TestServesExactlyTheSessionContract(t *testing.T) {
	for _, b := range []weavesession.Backend{&console{}, &full{console: &console{}}} {
		if err := weavemodule.CheckParity(weavesession.NewService(b)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCurrentReportsTheConsoleSession(t *testing.T) {
	b := &console{cur: alice(weavewire.SessionActive)}
	h := weavemoduletest.Start(t, weavesession.NewService(b))
	var got weavewire.SessionCurrentResponse
	h.Decode(weavewire.KindSessionCurrent, nil, &got)
	if got.Session == nil || got.Session.User != "alice" || !got.Session.Console {
		t.Fatalf("current = %+v", got.Session)
	}

	// Nobody logged in is an answer.
	b.set(nil, nil)
	got = weavewire.SessionCurrentResponse{}
	h.Decode(weavewire.KindSessionCurrent, nil, &got)
	if got.Session != nil {
		t.Fatalf("current = %+v, want nobody", got.Session)
	}

	b.set(nil, errors.New("no seat0"))
	if res := h.Call(weavewire.KindSessionCurrent, nil); !strings.Contains(res.Err, "no seat0") {
		t.Fatalf("err = %q", res.Err)
	}
}

func TestListAndLockAreUnsupportedWithoutTheBackend(t *testing.T) {
	h := weavemoduletest.Start(t, weavesession.NewService(&console{}))
	for _, kind := range []string{weavewire.KindSessionList, weavewire.KindSessionLock} {
		if res := h.Call(kind, nil); res.Code != weavewire.CodeUnsupported {
			t.Errorf("%s: %+v", kind, res)
		}
	}
}

func TestList(t *testing.T) {
	b := &full{console: &console{}, sessions: []weavewire.SessionInfo{*alice(weavewire.SessionLocked)}}
	h := weavemoduletest.Start(t, weavesession.NewService(b))
	var got weavewire.SessionListResponse
	h.Decode(weavewire.KindSessionList, nil, &got)
	if len(got.Sessions) != 1 || got.Sessions[0].State != weavewire.SessionLocked {
		t.Fatalf("list = %+v", got)
	}

	// None is an empty list, not null.
	b.sessions = nil
	res := h.Call(weavewire.KindSessionList, nil)
	if res.Err != "" || !strings.Contains(string(res.Payload), `"sessions":[]`) {
		t.Fatalf("res = %+v", res)
	}

	b.listErr = errors.New("wts failed")
	if res := h.Call(weavewire.KindSessionList, nil); !strings.Contains(res.Err, "wts failed") {
		t.Fatalf("err = %q", res.Err)
	}
}

func TestLock(t *testing.T) {
	b := &full{console: &console{cur: alice(weavewire.SessionActive)}}
	h := weavemoduletest.Start(t, weavesession.NewService(b))

	var got weavewire.SessionLockResponse
	h.Decode(weavewire.KindSessionLock, nil, &got)
	if got.SessionID != "2" {
		t.Fatalf("locked %q, want the console session", got.SessionID)
	}
	h.Decode(weavewire.KindSessionLock, weavewire.SessionLockRequest{SessionID: "7"}, &got)
	if got.SessionID != "7" || len(b.locked) != 2 || b.locked[1] != "7" {
		t.Fatalf("locked %v", b.locked)
	}

	b.set(nil, nil)
	if res := h.Call(weavewire.KindSessionLock, nil); !strings.Contains(res.Err, "nobody is logged in") {
		t.Fatalf("err = %q", res.Err)
	}
	b.set(nil, errors.New("probe failed"))
	if res := h.Call(weavewire.KindSessionLock, nil); !strings.Contains(res.Err, "probe failed") {
		t.Fatalf("err = %q", res.Err)
	}
	b.lockErr = errors.New("not permitted")
	if res := h.Call(weavewire.KindSessionLock, weavewire.SessionLockRequest{SessionID: "2"}); !strings.Contains(res.Err, "not permitted") {
		t.Fatalf("err = %q", res.Err)
	}
	if res := h.Call(weavewire.KindSessionLock, "garbage"); !strings.Contains(res.Err, "decoding") {
		t.Fatalf("err = %q", res.Err)
	}
}

func changes(sent []modulesdk.Message) []weavewire.SessionChangedEvent {
	var out []weavewire.SessionChangedEvent
	for _, m := range sent {
		if m.Kind != weavewire.KindSessionChanged {
			continue
		}
		var ev weavewire.SessionChangedEvent
		if json.Unmarshal(m.Data, &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

// The watcher reports logins, locks and logouts — and nothing for the state
// it started in, or for a probe that failed.
func TestChangesAreEmitted(t *testing.T) {
	b := &console{cur: alice(weavewire.SessionActive)}
	h := weavemoduletest.Start(t, weavesession.NewService(b, weavesession.WithPollInterval(5*time.Millisecond)))
	wait := func(n int) []weavewire.SessionChangedEvent {
		t.Helper()
		if !h.T.WaitFor(5*time.Second, func(s []modulesdk.Message) bool { return len(changes(s)) >= n }) {
			t.Fatalf("waited for %d changes, got %d", n, len(changes(h.T.Sent())))
		}
		return changes(h.T.Sent())
	}

	b.settle(t)
	if got := changes(h.T.Sent()); len(got) != 0 {
		t.Fatalf("the starting state was reported as a change: %+v", got)
	}

	b.set(alice(weavewire.SessionLocked), nil)
	ev := wait(1)[0]
	if ev.Previous.State != weavewire.SessionActive || ev.Current.State != weavewire.SessionLocked || ev.ChangedAt.IsZero() {
		t.Fatalf("lock event = %+v", ev)
	}

	// A failed probe is not a logout.
	b.set(nil, errors.New("probe failed"))
	b.settle(t)
	if got := changes(h.T.Sent()); len(got) != 1 {
		t.Fatalf("a failed probe was reported: %+v", got[1:])
	}

	b.set(nil, nil)
	ev = wait(2)[1]
	if ev.Previous == nil || ev.Current != nil {
		t.Fatalf("logout event = %+v", ev)
	}

	b.set(&weavewire.SessionInfo{ID: "3", User: "bob", State: weavewire.SessionActive}, nil)
	ev = wait(3)[2]
	if ev.Previous != nil || ev.Current.User != "bob" || !ev.Current.Console {
		t.Fatalf("login event = %+v", ev)
	}
}

// A watcher that cannot read the console at first takes its first good
// reading as the baseline, and a change the host was not there to receive
// does not stop the next one.
func TestTheBaselineWaitsForAGoodReading(t *testing.T) {
	b := &console{err: errors.New("not yet")}
	var mu sync.Mutex
	refused := 0
	h := weavemoduletest.Start(t,
		weavesession.NewService(b, weavesession.WithPollInterval(5*time.Millisecond), weavesession.WithPollInterval(0)),
		func(host *weavemoduletest.Host) {
			host.T.OnSend = func(msg modulesdk.Message) error {
				mu.Lock()
				defer mu.Unlock()
				if msg.Kind == weavewire.KindSessionChanged && refused == 0 {
					refused++
					return errors.New("no host")
				}
				return nil
			}
		})
	b.settle(t)
	b.set(alice(weavewire.SessionActive), nil) // the baseline
	b.settle(t)
	b.set(alice(weavewire.SessionLocked), nil) // refused: nobody listening
	b.settle(t)
	b.set(nil, nil) // delivered
	if !h.T.WaitFor(5*time.Second, func(s []modulesdk.Message) bool { return len(changes(s)) == 1 }) {
		t.Fatalf("events = %+v", changes(h.T.Sent()))
	}
	ev := changes(h.T.Sent())[0]
	if ev.Previous == nil || ev.Previous.State != weavewire.SessionLocked || ev.Current != nil {
		t.Fatalf("event = %+v", ev)
	}
}
