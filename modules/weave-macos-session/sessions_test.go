//go:build darwin

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Entries in the shape IOConsoleUsers holds them (see `ioreg -n Root -d1`):
// the console user, a fast-user-switched session behind the login window, one
// still logging in, and loginwindow's own.
func aliceEntry() consoleEntry {
	return consoleEntry{
		"CGSSessionUniqueSessionUUID": "07DA50BA-7518-4BF0-90C6-B2BD611FC0E3",
		keyAuditID:                    int64(100021),
		"kCGSSessionGroupIDKey":       int64(20),
		"kCGSSessionIDKey":            int64(257),
		keyOnConsole:                  int64(1),
		keyUID:                        int64(501),
		keyUser:                       "alice",
		keyLoginDone:                  int64(1),
		"kCGSessionLongUserNameKey":   "Alice Example",
		"kSCSecuritySessionID":        int64(100021),
	}
}

func bobEntry() consoleEntry {
	return consoleEntry{
		keyAuditID: int64(100044), keyOnConsole: int64(0), keyUID: int64(502),
		keyUser: "bob", keyLoginDone: int64(1),
	}
}

func carolEntry() consoleEntry {
	return consoleEntry{
		keyAuditID: int64(100050), keyOnConsole: int64(0), keyUID: int64(503),
		keyUser: "carol", keyLoginDone: int64(0),
	}
}

func loginwindowEntry() consoleEntry {
	return consoleEntry{
		keyAuditID: int64(100001), keyOnConsole: int64(1), keyUID: int64(0),
		keyUser: "loginwindow", keyLoginDone: int64(1),
	}
}

func with(e consoleEntry, k string, v any) consoleEntry {
	e[k] = v
	return e
}

func stub(name string, uid uint32, present bool, entries []consoleEntry, err error) sessions {
	return sessions{
		consoleUser:  func() (string, uint32, bool) { return name, uid, present },
		consoleUsers: func() ([]consoleEntry, error) { return entries, err },
	}
}

func TestConsole(t *testing.T) {
	errIOKit := errors.New("IOKit failed")
	active := weavewire.SessionInfo{
		ID:    "100021",
		User:  "alice",
		UID:   "501",
		State: weavewire.SessionActive,
	}
	locked := active
	locked.State = weavewire.SessionLocked
	for name, tc := range map[string]struct {
		s       sessions
		want    *weavewire.SessionInfo
		wantErr error
	}{
		"alice at the console": {
			s:    stub("alice", 501, true, []consoleEntry{bobEntry(), aliceEntry()}, nil),
			want: &active,
		},
		"alice's screen locked": {
			s:    stub("alice", 501, true, []consoleEntry{with(aliceEntry(), keyLocked, int64(1))}, nil),
			want: &locked,
		},
		"nobody":         {s: stub("", 0, false, nil, nil)},
		"login window":   {s: stub("loginwindow", 0, true, []consoleEntry{loginwindowEntry()}, nil)},
		"setup":          {s: stub("_mbsetupuser", 248, true, nil, nil)},
		"root":           {s: stub("root", 0, true, nil, nil)},
		"IOKit fails":    {s: stub("alice", 501, true, nil, errIOKit), wantErr: errIOKit},
		"no entry yet":   {s: stub("alice", 501, true, []consoleEntry{bobEntry()}, nil), wantErr: errNoEntry},
		"switched entry": {s: stub("alice", 501, true, []consoleEntry{with(aliceEntry(), keyOnConsole, int64(0))}, nil), wantErr: errNoEntry},
		"entry without an audit id": {
			s:       stub("alice", 501, true, []consoleEntry{with(aliceEntry(), keyAuditID, "x")}, nil),
			wantErr: errNoEntry,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok, err := tc.s.Console(context.Background())
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			switch {
			case tc.want == nil && ok:
				t.Errorf("console = %+v, want nobody", got)
			case tc.want != nil && (!ok || got != *tc.want):
				t.Errorf("console = %+v (%v), want %+v", got, ok, *tc.want)
			}
		})
	}
}

func TestList(t *testing.T) {
	s := stub("alice", 501, true, []consoleEntry{
		bobEntry(), carolEntry(), loginwindowEntry(), with(aliceEntry(), keyLocked, int64(1)),
		{keyUser: "dave"}, // no ids at all
	}, nil)
	got, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []weavewire.SessionInfo{
		{ID: "100021", User: "alice", UID: "501", State: weavewire.SessionLocked, Console: true},
		{ID: "100044", User: "bob", UID: "502", State: weavewire.SessionInactive},
	}
	if len(got) != len(want) {
		t.Fatalf("list = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("session %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// At the login window nobody listed is at the console, even an entry
	// still flagged on it.
	got, err = stub(
		"loginwindow",
		0,
		true,
		[]consoleEntry{aliceEntry()},
		nil,
	).List(context.Background())
	if err != nil || len(got) != 1 || got[0].Console {
		t.Errorf("list at the login window = %+v, %v", got, err)
	}

	errIOKit := errors.New("IOKit failed")
	if _, err := stub(
		"alice",
		501,
		true,
		nil,
		errIOKit,
	).List(context.Background()); !errors.Is(
		err,
		errIOKit,
	) {
		t.Errorf("err = %v, want IOKit's", err)
	}
}

func TestEntryAccessors(t *testing.T) {
	e := consoleEntry{"n": int64(3), "s": "x", "b": int64(0)}
	if v, ok := e.int("n"); !ok || v != 3 {
		t.Errorf("int = %d, %v", v, ok)
	}
	if _, ok := e.int("s"); ok {
		t.Error("a string read as a number")
	}
	if e.flag("b") || !e.flag("n") || e.flag("missing") {
		t.Error("flags misread")
	}
	if e.str("s") != "x" || e.str("n") != "" {
		t.Error("strings misread")
	}
}
