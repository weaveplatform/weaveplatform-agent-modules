//go:build darwin

package main

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// The keys of an IOConsoleUsers entry, from IOKitKeys.h (kIOConsoleSession*).
// loginwindow keeps one entry per GUI login session, fast-user-switched ones
// included, and IOKit publishes the array to every process.
const (
	keyAuditID   = "kCGSSessionAuditIDKey"
	keyUID       = "kCGSSessionUserIDKey"
	keyUser      = "kCGSSessionUserNameKey"
	keyOnConsole = "kCGSSessionOnConsoleKey"
	keyLoginDone = "kCGSessionLoginDoneKey"
	keyLocked    = "CGSSessionScreenIsLocked"
)

// consoleEntry is one IOConsoleUsers entry: numbers (booleans included, as
// CFBoolean is an NSNumber) as int64 and strings as string.
type consoleEntry map[string]any

func (e consoleEntry) int(key string) (int64, bool) {
	v, ok := e[key].(int64)
	return v, ok
}

func (e consoleEntry) flag(key string) bool {
	v, _ := e.int(key)
	return v != 0
}

func (e consoleEntry) str(key string) string {
	v, _ := e[key].(string)
	return v
}

// sessions reports macOS GUI login sessions.
//
// Who is at the console comes from SCDynamicStoreCopyConsoleUser, as agent-core
// asks it, so this module never disagrees with core about the session core
// starts the console modules in. Everything else — the audit session id the
// contract names, whether the screen is locked, the sessions switched away
// from — comes from IOKit's IOConsoleUsers. Both are readable by any process,
// unlike CGSessionCopyCurrentDictionary, which only answers a caller inside
// the GUI session itself, which this module is not.
//
// The two reads are fields so tests drive every decision with any answer.
type sessions struct {
	consoleUser  func() (name string, uid uint32, present bool)
	consoleUsers func() ([]consoleEntry, error)
}

func newSessions() sessions {
	return sessions{consoleUser: copyConsoleUser, consoleUsers: readConsoleUsers}
}

// errNoEntry reports a console user loginwindow has not (yet) published an
// IOConsoleUsers entry for.
var errNoEntry = errors.New("IOConsoleUsers has no entry for the console user")

// Console implements weavesession.Backend.
func (s sessions) Console(context.Context) (weavewire.SessionInfo, bool, error) {
	name, uid, present := s.consoleUser()
	if !person(name, int64(uid)) || !present {
		return weavewire.SessionInfo{}, false, nil
	}
	entries, err := s.consoleUsers()
	if err != nil {
		return weavewire.SessionInfo{}, false, err
	}
	for _, e := range entries {
		if u, ok := e.int(keyUID); ok && u == int64(uid) && e.flag(keyOnConsole) {
			info, ok := sessionInfo(e)
			if !ok {
				break
			}
			// The console user's name as SystemConfiguration has it: the
			// short name, which is also what the entry should carry.
			info.User = name
			return info, true, nil
		}
	}
	// Mid-login, the console user can be published before its session
	// entry. An error rather than a guessed id: the service keeps its last
	// reading, and reports the session once it has its real id.
	return weavewire.SessionInfo{}, false, errNoEntry
}

// List implements weavesession.Lister: every GUI login session that has
// finished logging in, at the console or switched away from.
func (s sessions) List(context.Context) ([]weavewire.SessionInfo, error) {
	entries, err := s.consoleUsers()
	if err != nil {
		return nil, err
	}
	name, uid, present := s.consoleUser()
	out := make([]weavewire.SessionInfo, 0, len(entries))
	for _, e := range entries {
		info, ok := sessionInfo(e)
		if !ok || !e.flag(keyLoginDone) {
			continue
		}
		info.Console = present && e.flag(keyOnConsole) &&
			info.UID == strconv.FormatUint(uint64(uid), 10) && person(name, int64(uid))
		out = append(out, info)
	}
	slices.SortFunc(
		out,
		func(a, b weavewire.SessionInfo) int { return strings.Compare(a.ID, b.ID) },
	)
	return out, nil
}

// sessionInfo reads one entry, reporting false for one that is not a person's
// session.
func sessionInfo(e consoleEntry) (weavewire.SessionInfo, bool) {
	audit, okA := e.int(keyAuditID)
	uid, okU := e.int(keyUID)
	user := e.str(keyUser)
	if !okA || !okU || !person(user, uid) {
		return weavewire.SessionInfo{}, false
	}
	info := weavewire.SessionInfo{
		ID:    strconv.FormatInt(audit, 10),
		User:  user,
		UID:   strconv.FormatInt(uid, 10),
		State: weavewire.SessionInactive,
	}
	switch {
	case e.flag(keyLocked):
		info.State = weavewire.SessionLocked
	case e.flag(keyOnConsole):
		info.State = weavewire.SessionActive
	}
	return info, true
}

// person reports whether a console user is someone whose desktop a module
// should touch. At the login window the console "user" is loginwindow (or
// nobody), and during Setup Assistant it is _mbsetupuser; uid 0 is root at
// the console, which a per-user module must never run as. The same filter as
// agent-core's, so the two agree.
func person(name string, uid int64) bool {
	return name != "" && name != "loginwindow" && name != "_mbsetupuser" && uid > 0
}
