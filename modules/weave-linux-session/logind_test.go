//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Session records in the shape logind's session_save writes them, trimmed to
// a realistic subset: a GNOME Wayland login on seat0, its gdm greeter, an SSH
// login, and one on its way out.
const (
	userSession = `# This is private data. Do not parse.
UID=1000
USER=alice
ACTIVE=1
IS_DISPLAY=1
STATE=active
REMOTE=0
LEADER_FD_SAVED=1
TYPE=wayland
ORIGINAL_TYPE=wayland
CLASS=user
SCOPE=session-2.scope
SEAT=seat0
TTY=tty2
TTY_VALIDITY=from-pam
SERVICE=gdm-password
DESKTOP=GNOME
VTNR=2
LEADER=1712
AUDIT=2
REALTIME=1759480245123456
MONOTONIC=36812345
`
	greeterSession = `UID=120
USER=gdm
ACTIVE=1
STATE=active
REMOTE=0
TYPE=wayland
CLASS=greeter
SEAT=seat0
VTNR=1
REALTIME=1759480200000000
`
	sshSession = `UID=1001
USER="bob smith"
ACTIVE=1
STATE=online
REMOTE=1
TYPE=tty
CLASS=user
REMOTE_HOST=192.0.2.7
SERVICE=sshd
REALTIME=1759480300000000
`
	closingSession = `UID=1002
USER=carol
STATE=closing
REMOTE=0
CLASS=user
`
	managerSession = `UID=1000
USER=alice
STATE=active
CLASS=manager
`
)

// fixture lays out a /run/systemd tree under a temp root.
func fixture(t *testing.T, active string, sessions map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"run/systemd/seats", "run/systemd/sessions"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if active != "-" {
		seat := "ACTIVE=" + active + "\nACTIVE_UID=1000\nSESSIONS=" + active + "\n"
		write(t, filepath.Join(root, "run/systemd/seats/seat0"), seat)
	}
	for id, body := range sessions {
		write(t, filepath.Join(root, "run/systemd/sessions", id), body)
	}
	return root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeLoginctl answers LockedHint for the ids in locked and records every
// call; any other request fails with err.
type fakeLoginctl struct {
	locked map[string]bool
	err    error
	calls  [][]string
}

func (f *fakeLoginctl) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	if args[0] == "show-session" {
		if f.locked[args[1]] {
			return []byte("yes\n"), nil
		}
		return []byte("no\n"), nil
	}
	return nil, nil
}

func newFixtureLogind(root string, f *fakeLoginctl) logind {
	return logind{root: root, seat: "seat0", loginctl: f.run}
}

func TestConsole(t *testing.T) {
	ctx := context.Background()
	alice := weavewire.SessionInfo{
		ID: "2", User: "alice", UID: "1000", State: weavewire.SessionActive,
		Since: time.Date(2025, 10, 3, 8, 30, 45, 123456000, time.UTC),
	}
	lockedAlice := alice
	lockedAlice.State = weavewire.SessionLocked
	for name, tc := range map[string]struct {
		active   string
		sessions map[string]string
		locked   map[string]bool
		want     *weavewire.SessionInfo
		wantErr  bool
	}{
		"a user at the console": {
			active: "2", sessions: map[string]string{"2": userSession, "c1": greeterSession},
			want: &alice,
		},
		"locked": {
			active: "2", sessions: map[string]string{"2": userSession},
			locked: map[string]bool{"2": true}, want: &lockedAlice,
		},
		"the greeter":           {active: "c1", sessions: map[string]string{"c1": greeterSession}},
		"no seat file":          {active: "-"},
		"no active session":     {active: "", sessions: map[string]string{"2": userSession}},
		"an id that is a path":  {active: "../2", sessions: map[string]string{"2": userSession}},
		"the session just went": {active: "7"},
		"remote":                {active: "5", sessions: map[string]string{"5": strings.Replace(userSession, "REMOTE=0", "REMOTE=1", 1)}},
		"switched away":         {active: "2", sessions: map[string]string{"2": strings.Replace(userSession, "STATE=active", "STATE=online", 1)}},
		"bad uid": {
			active: "2", sessions: map[string]string{"2": strings.Replace(userSession, "UID=1000", "UID=x", 1)},
			wantErr: true,
		},
		"no user": {
			active: "2", sessions: map[string]string{"2": strings.Replace(userSession, "USER=alice", "USER=", 1)},
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := fixture(t, tc.active, tc.sessions)
			l := newFixtureLogind(root, &fakeLoginctl{locked: tc.locked})
			got, ok, err := l.Console(ctx)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
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

// A state directory that cannot be read is a failure to report, not "nobody
// is logged in": the service then keeps its last known state.
func TestConsoleReportsUnreadableState(t *testing.T) {
	root := fixture(t, "-", nil)
	if err := os.Mkdir(filepath.Join(root, "run/systemd/seats/seat0"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := newFixtureLogind(root, &fakeLoginctl{})
	if _, _, err := l.Console(context.Background()); err == nil {
		t.Error("an unreadable seat file read as nobody at the console")
	}

	root = fixture(t, "2", nil)
	if err := os.Mkdir(filepath.Join(root, "run/systemd/sessions/2"), 0o755); err != nil {
		t.Fatal(err)
	}
	l = newFixtureLogind(root, &fakeLoginctl{})
	if _, _, err := l.Console(context.Background()); err == nil {
		t.Error("an unreadable session file read as nobody at the console")
	}
}

// Without loginctl the lock state is unknown, and an active session reads as
// active — what logind's own files say.
func TestConsoleWithoutLoginctl(t *testing.T) {
	root := fixture(t, "2", map[string]string{"2": userSession})
	l := newFixtureLogind(root, &fakeLoginctl{err: errors.New("loginctl: not found")})
	got, ok, err := l.Console(context.Background())
	if err != nil || !ok || got.State != weavewire.SessionActive {
		t.Fatalf("console = %+v, %v, %v", got, ok, err)
	}
}

func TestList(t *testing.T) {
	root := fixture(t, "2", map[string]string{
		"2": userSession, "c1": greeterSession, "5": sshSession, "9": closingSession,
		"3": managerSession, "6": "UID=oops\nUSER=x\nCLASS=user\nSTATE=active\n",
	})
	// The FIFO logind keeps beside each session, and a stray directory.
	write(t, filepath.Join(root, "run/systemd/sessions/2.ref"), "")
	if err := os.Mkdir(filepath.Join(root, "run/systemd/sessions/7"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := newFixtureLogind(root, &fakeLoginctl{locked: map[string]bool{"2": true}})
	got, err := l.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []string{"2", "5"}) {
		t.Fatalf("listed %v, want the console login and the SSH one", ids)
	}
	if s := got[0]; !s.Console || s.State != weavewire.SessionLocked || s.Remote {
		t.Errorf("console session = %+v", s)
	}
	if s := got[1]; s.Console || !s.Remote || s.State != weavewire.SessionInactive ||
		s.User != "bob smith" || s.UID != "1001" {
		t.Errorf("ssh session = %+v", s)
	}
}

func TestListWithoutLogind(t *testing.T) {
	l := newFixtureLogind(t.TempDir(), &fakeLoginctl{})
	got, err := l.List(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("list = %v, %v; want nothing", got, err)
	}
}

func TestListReportsUnreadableState(t *testing.T) {
	root := fixture(t, "-", nil)
	sessions := filepath.Join(root, "run/systemd/sessions")
	if err := os.Remove(sessions); err != nil {
		t.Fatal(err)
	}
	write(t, sessions, "not a directory")
	if _, err := newFixtureLogind(root, &fakeLoginctl{}).List(context.Background()); err == nil {
		t.Error("an unreadable sessions directory listed as empty")
	}

	root = fixture(t, "-", map[string]string{"2": userSession})
	if err := os.Mkdir(filepath.Join(root, "run/systemd/seats/seat0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newFixtureLogind(root, &fakeLoginctl{}).List(context.Background()); err == nil {
		t.Error("an unreadable seat file listed without a console")
	}
}

func TestLock(t *testing.T) {
	f := &fakeLoginctl{}
	l := newFixtureLogind(t.TempDir(), f)
	if err := l.Lock(context.Background(), "c2"); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"lock-session", "c2"}}; !slices.EqualFunc(f.calls, want, slices.Equal) {
		t.Errorf("loginctl calls = %v, want %v", f.calls, want)
	}

	for _, id := range []string{"", "-h", "--all", "2;reboot", "../2", "a b"} {
		if err := l.Lock(context.Background(), id); !errors.Is(err, errBadID) {
			t.Errorf("lock %q: err = %v, want errBadID", id, err)
		}
	}
	if len(f.calls) != 1 {
		t.Errorf("a bad id reached loginctl: %v", f.calls)
	}

	errLogind := errors.New("logind said no")
	l = newFixtureLogind(t.TempDir(), &fakeLoginctl{err: errLogind})
	if err := l.Lock(context.Background(), "2"); !errors.Is(err, errLogind) {
		t.Errorf("err = %v, want logind's refusal", err)
	}
}

func TestParseEnv(t *testing.T) {
	got := parseEnv("# comment\n\nA=1\nB=\"two words\"\nC=\"\nnot a pair\nD=x=y\n")
	want := map[string]string{"A": "1", "B": "two words", "C": `"`, "D": "x=y"}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// fakeTool puts an executable named name on a PATH of its own, so the real
// exec path runs without the real tool.
func fakeTool(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, name),
		[]byte("#!/bin/sh\n"+script),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestRunLoginctl(t *testing.T) {
	ctx := context.Background()

	fakeTool(t, "loginctl", `echo "$@"`)
	out, err := runLoginctl(ctx, "show-session", "2", "--property=LockedHint", "--value")
	if err != nil ||
		strings.TrimSpace(string(out)) != "show-session 2 --property=LockedHint --value" {
		t.Errorf("out = %q, err = %v", out, err)
	}

	fakeTool(t, "loginctl", "echo 'Failed to issue method call: Access denied' >&2; exit 1\n")
	if _, err := runLoginctl(ctx, "lock-session", "2"); err == nil ||
		!strings.Contains(err.Error(), "Access denied") {
		t.Errorf("err = %v, want loginctl's message", err)
	}

	fakeTool(t, "loginctl", "exit 3\n")
	if _, err := runLoginctl(ctx, "lock-session", "2"); err == nil {
		t.Error("a failing loginctl succeeded")
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := runLoginctl(ctx, "lock-session", "2"); err == nil {
		t.Error("a missing loginctl succeeded")
	}
}

// The real state directory of whatever runs the test: a CI runner or
// container has no seat, a desktop has one. Either answer is real; an error
// is not.
func TestRealLogind(t *testing.T) {
	l := newLogind()
	info, ok, err := l.Console(context.Background())
	if err != nil {
		t.Fatalf("console: %v", err)
	}
	t.Logf("console: %+v (%v)", info, ok)
	sessions, err := l.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	t.Logf("sessions: %+v", sessions)
}
