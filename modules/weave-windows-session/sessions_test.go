//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"os/user"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/remotedesktop"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

func utf16Bytes(s string) []byte {
	var b []byte
	for _, u := range append(utf16.Encode([]rune(s)), 0) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

// logon1 is 2026-10-03 08:30:45 UTC as a FILETIME.
var logon = time.Date(2026, 10, 3, 8, 30, 45, 0, time.UTC)

func logonFiletime() int64 { return logon.UnixNano()/100 + filetimeEpochDelta }

// infoEx lays out a WTSINFOEXW buffer as Terminal Services returns it.
func infoEx(level uint32, state remotedesktop.WTS_CONNECTSTATE_CLASS, flags int32) []byte {
	var ex remotedesktop.WTSINFOEXW
	ex.Level = level
	l1 := (*remotedesktop.WTSINFOEX_LEVEL1_W)(unsafe.Pointer(&ex.Data))
	l1.SessionState = state
	l1.SessionFlags = flags
	l1.LogonTime = logonFiletime()
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(&ex)), unsafe.Sizeof(ex))...)
}

// session is one stand-in session's answers to each information class.
type session map[remotedesktop.WTS_INFO_CLASS][]byte

var errNoSuchSession = errors.New("no such session")

func stub(console uint32, sessions map[uint32]session, enumErr error) wts {
	return wts{
		activeConsole: func() uint32 { return console },
		query: func(id uint32, class remotedesktop.WTS_INFO_CLASS) ([]byte, error) {
			s, ok := sessions[id]
			if !ok {
				return nil, errNoSuchSession
			}
			raw, ok := s[class]
			if !ok {
				return nil, errors.New("class refused")
			}
			return raw, nil
		},
		enumerate: func() ([]uint32, error) {
			ids := []uint32{0}
			for id := range sessions {
				ids = append(ids, id)
			}
			return ids, enumErr
		},
		sid: func(account string) (string, error) {
			if account == `WS01\alice` {
				return "S-1-5-21-1-2-3-1001", nil
			}
			return "", errors.New("unknown account")
		},
	}
}

func alice() session {
	return session{
		remotedesktop.WTSUserName:   utf16Bytes("alice"),
		remotedesktop.WTSDomainName: utf16Bytes("WS01"),
		remotedesktop.WTSSessionInfoEx: infoEx(
			1,
			remotedesktop.WTSActive,
			int32(remotedesktop.WTS_SESSIONSTATE_UNLOCK),
		),
		remotedesktop.WTSClientProtocolType: {0, 0},
	}
}

func with(s session, class remotedesktop.WTS_INFO_CLASS, raw []byte) session {
	if raw == nil {
		delete(s, class)
	} else {
		s[class] = raw
	}
	return s
}

func TestConsole(t *testing.T) {
	want := weavewire.SessionInfo{
		ID: "1", User: `WS01\alice`, UID: "S-1-5-21-1-2-3-1001",
		State: weavewire.SessionActive, Since: logon,
	}
	locked := want
	locked.State = weavewire.SessionLocked
	bare := weavewire.SessionInfo{ID: "1", User: "alice", State: weavewire.SessionActive}
	for name, tc := range map[string]struct {
		w       wts
		want    *weavewire.SessionInfo
		wantErr bool
	}{
		"alice":    {w: stub(1, map[uint32]session{1: alice()}, nil), want: &want},
		"locked":   {w: stub(1, map[uint32]session{1: with(alice(), remotedesktop.WTSSessionInfoEx, infoEx(1, remotedesktop.WTSActive, 0))}, nil), want: &locked},
		"detached": {w: stub(noConsoleSession, nil, nil)},
		"services": {w: stub(0, nil, nil)},
		"logon screen": {
			w: stub(1, map[uint32]session{1: with(alice(), remotedesktop.WTSUserName, utf16Bytes(""))}, nil),
		},
		"user name refused": {w: stub(1, map[uint32]session{1: with(alice(), remotedesktop.WTSUserName, nil)}, nil), wantErr: true},
		// Every best-effort field refused, or answered with something
		// unreadable: the session is still reported, with what is known.
		"only a name": {
			w: stub(1, map[uint32]session{1: {
				remotedesktop.WTSUserName:           utf16Bytes("alice"),
				remotedesktop.WTSSessionInfoEx:      infoEx(2, remotedesktop.WTSActive, 0),
				remotedesktop.WTSClientProtocolType: {0},
			}}, nil),
			want: &bare,
		},
		"short info record": {
			w: stub(1, map[uint32]session{1: {
				remotedesktop.WTSUserName:      utf16Bytes("alice"),
				remotedesktop.WTSDomainName:    utf16Bytes(""),
				remotedesktop.WTSSessionInfoEx: {1, 0, 0, 0},
			}}, nil),
			want: &bare,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok, err := tc.w.Console(context.Background())
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

func TestList(t *testing.T) {
	rdp := session{
		remotedesktop.WTSUserName: utf16Bytes("bob"),
		remotedesktop.WTSSessionInfoEx: infoEx(
			1,
			remotedesktop.WTSActive,
			int32(remotedesktop.WTS_SESSIONSTATE_UNLOCK),
		),
		remotedesktop.WTSClientProtocolType: {2, 0},
	}
	switched := session{
		remotedesktop.WTSUserName:      utf16Bytes("carol"),
		remotedesktop.WTSSessionInfoEx: infoEx(1, remotedesktop.WTSDisconnected, -1),
	}
	listener := session{remotedesktop.WTSUserName: utf16Bytes("")}
	gone := session{} // logged off before its name could be read
	w := stub(1, map[uint32]session{1: alice(), 2: rdp, 3: switched, 65536: listener, 4: gone}, nil)
	got, err := w.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("list = %+v, want alice, bob and carol", got)
	}
	if s := got[0]; s.ID != "1" || !s.Console || s.Remote || s.State != weavewire.SessionActive {
		t.Errorf("console session = %+v", s)
	}
	if s := got[1]; s.ID != "2" || s.Console || !s.Remote || s.User != "bob" {
		t.Errorf("rdp session = %+v", s)
	}
	if s := got[2]; s.ID != "3" || s.Console || s.State != weavewire.SessionInactive {
		t.Errorf("switched session = %+v", s)
	}

	errEnum := errors.New("enumeration refused")
	if _, err := stub(1, nil, errEnum).List(context.Background()); !errors.Is(err, errEnum) {
		t.Errorf("err = %v, want the enumeration's", err)
	}
}

func TestFiletime(t *testing.T) {
	if got := filetime(logonFiletime()); !got.Equal(logon) {
		t.Errorf("filetime = %v, want %v", got, logon)
	}
	if got := filetime(0); !got.IsZero() {
		t.Errorf("a never-logged-on time read as %v", got)
	}
}

func TestUTF16String(t *testing.T) {
	for raw, want := range map[string]string{
		string(utf16Bytes("Zoë")):            "Zoë",
		string(append(utf16Bytes("a"), 'x')): "a",
		"":                                   "",
		string([]byte{'h', 0, 'i', 0}):       "hi", // no terminator
	} {
		if got := utf16String([]byte(raw)); got != want {
			t.Errorf("utf16String(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Against the real Terminal Services API of whatever runs the test. A CI
// runner's service may run in session 0 with or without a console user; any
// answer is a real one, and only an error is a broken call.
func TestRealSessions(t *testing.T) {
	w := newWTS()
	info, ok, err := w.Console(context.Background())
	if err != nil {
		t.Fatalf("console: %v", err)
	}
	t.Logf("console: %+v (%v)", info, ok)
	list, err := w.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	t.Logf("sessions: %+v", list)
	if ok && info.UID == "" {
		t.Errorf("console user %q has no SID", info.User)
	}
	if _, err := querySession(0xFFFFFFF0, remotedesktop.WTSUserName); err == nil {
		t.Error("a session that does not exist answered")
	}
}

// The SID lookup against this process's own account, whose SID os/user
// already knows.
func TestAccountSID(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	got, err := accountSID(me.Username)
	if err != nil {
		t.Fatal(err)
	}
	if got != me.Uid {
		t.Errorf("SID of %s = %s, want %s", me.Username, got, me.Uid)
	}
	if _, err := accountSID(`NOSUCHDOMAIN\nosuchuser`); err == nil {
		t.Error("an unknown account resolved")
	}
}
