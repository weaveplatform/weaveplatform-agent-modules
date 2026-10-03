//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/remotedesktop"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// noConsoleSession is what WTSGetActiveConsoleSessionId returns while the
// console is detached — mid-switch between sessions.
const noConsoleSession = 0xFFFFFFFF

// wts reports Windows logon sessions through the Terminal Services API, which
// covers the console, fast-user-switched sessions and Remote Desktop alike.
//
// The console comes from WTSGetActiveConsoleSessionId and the user logged on
// to it, as agent-core reads it, so this module and core agree on the session
// core starts the console modules in.
//
// The OS calls are fields so tests drive every decision with any answer.
type wts struct {
	activeConsole func() uint32
	// query returns a copy of WTSQuerySessionInformation's buffer.
	query func(id uint32, class remotedesktop.WTS_INFO_CLASS) ([]byte, error)
	// enumerate lists the ids of every session on this machine.
	enumerate func() ([]uint32, error)
	// sid resolves an account name (DOMAIN\user) to its SID string.
	sid func(account string) (string, error)
}

func newWTS() wts {
	return wts{
		activeConsole: remotedesktop.WTSGetActiveConsoleSessionId,
		query:         querySession,
		enumerate:     enumerateSessions,
		sid:           accountSID,
	}
}

// Console implements weavesession.Backend.
func (w wts) Console(context.Context) (weavewire.SessionInfo, bool, error) {
	id := w.activeConsole()
	// Session 0 is services-only since Vista; it never has a desktop user.
	if id == noConsoleSession || id == 0 {
		return weavewire.SessionInfo{}, false, nil
	}
	return w.info(id)
}

// List implements weavesession.Lister: every session with a user logged on,
// at the console, switched away from or remote.
func (w wts) List(context.Context) ([]weavewire.SessionInfo, error) {
	ids, err := w.enumerate()
	if err != nil {
		return nil, err
	}
	console := w.activeConsole()
	out := make([]weavewire.SessionInfo, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		info, ok, err := w.info(id)
		if err != nil || !ok {
			continue // logged off between the enumeration and the query
		}
		info.Console = id == console
		out = append(out, info)
	}
	slices.SortFunc(
		out,
		func(a, b weavewire.SessionInfo) int { return strings.Compare(a.ID, b.ID) },
	)
	return out, nil
}

// info describes one session, reporting false when nobody is logged on to it
// (the console at the logon screen, an RDP listener).
//
// Only the user name is required. The lock state, logon time, protocol and
// SID are each best-effort, so a query Windows refuses costs a field rather
// than the session.
func (w wts) info(id uint32) (weavewire.SessionInfo, bool, error) {
	raw, err := w.query(id, remotedesktop.WTSUserName)
	if err != nil {
		return weavewire.SessionInfo{}, false, err
	}
	user := utf16String(raw)
	if user == "" {
		return weavewire.SessionInfo{}, false, nil
	}
	if raw, err := w.query(id, remotedesktop.WTSDomainName); err == nil {
		if domain := utf16String(raw); domain != "" {
			user = domain + `\` + user
		}
	}
	info := weavewire.SessionInfo{
		ID:    strconv.FormatUint(uint64(id), 10),
		User:  user,
		State: weavewire.SessionActive,
	}
	if raw, err := w.query(id, remotedesktop.WTSSessionInfoEx); err == nil {
		if ex, ok := sessionInfoEx(raw); ok {
			info.State = state(ex)
			info.Since = filetime(ex.LogonTime)
		}
	}
	// WTSClientProtocolType is a USHORT: 0 for the console, 2 for RDP.
	if raw, err := w.query(id, remotedesktop.WTSClientProtocolType); err == nil && len(raw) >= 2 {
		info.Remote = uint32(
			binary.LittleEndian.Uint16(raw),
		) != remotedesktop.WTS_PROTOCOL_TYPE_CONSOLE
	}
	if sid, err := w.sid(user); err == nil {
		info.UID = sid
	}
	return info, true, nil
}

// sessionInfoEx reads the level-1 record from a WTSSessionInfoEx buffer.
func sessionInfoEx(raw []byte) (remotedesktop.WTSINFOEX_LEVEL1_W, bool) {
	var ex remotedesktop.WTSINFOEXW
	if len(raw) < int(unsafe.Sizeof(ex)) {
		return remotedesktop.WTSINFOEX_LEVEL1_W{}, false
	}
	ex = *(*remotedesktop.WTSINFOEXW)(unsafe.Pointer(&raw[0]))
	if ex.Level != 1 {
		return remotedesktop.WTSINFOEX_LEVEL1_W{}, false
	}
	return *(*remotedesktop.WTSINFOEX_LEVEL1_W)(unsafe.Pointer(&ex.Data)), true
}

// state maps a session's connection state and lock flag.
//
// SessionFlags is WTS_SESSIONSTATE_LOCK (0), WTS_SESSIONSTATE_UNLOCK (1) or
// WTS_SESSIONSTATE_UNKNOWN (-1); the last reads as unlocked rather than as a
// lock nobody asked about.
func state(ex remotedesktop.WTSINFOEX_LEVEL1_W) weavewire.SessionState {
	switch {
	case ex.SessionState != remotedesktop.WTSActive:
		// Disconnected: switched away from with fast user switching, or an
		// RDP client that went away. Either way, in front of nobody.
		return weavewire.SessionInactive
	case uint32(ex.SessionFlags) == remotedesktop.WTS_SESSIONSTATE_LOCK: //nolint:gosec // G115: -1 is not LOCK either way.
		return weavewire.SessionLocked
	default:
		return weavewire.SessionActive
	}
}

// filetimeEpochDelta is the number of 100 ns intervals between 1601-01-01,
// FILETIME's epoch, and 1970-01-01.
const filetimeEpochDelta = 116444736000000000

// filetime converts a FILETIME count; zero (never) stays the zero time.
func filetime(ft int64) time.Time {
	if ft <= filetimeEpochDelta {
		return time.Time{}
	}
	return time.Unix(0, (ft-filetimeEpochDelta)*100).UTC()
}

// utf16String decodes a NUL-terminated UTF-16 buffer.
func utf16String(raw []byte) string {
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	if i := slices.Index(units, 0); i >= 0 {
		units = units[:i]
	}
	return string(utf16.Decode(units))
}
