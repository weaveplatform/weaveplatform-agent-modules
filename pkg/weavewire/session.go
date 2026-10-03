package weavewire

import "time"

// The session capability reports who is logged in and locks a session. Its
// modules run as system (PlacementSystem), so it answers whether or not
// anyone is logged in — which is what lets a host tell "nobody at the
// console" from "no agent" when a per-user-console capability stays silent.
//
// Configuring autologon is provisioning — it changes how the machine boots,
// not the session in front of it — and unlocking needs the user's
// credentials, so neither is an op here.

const (
	// KindSessionCurrent reports the session at the physical console.
	KindSessionCurrent = "weave.session.current"
	// KindSessionList reports every logged-in session the OS can enumerate.
	KindSessionList = "weave.session.list"
	// KindSessionLock locks a session's screen.
	KindSessionLock = "weave.session.lock"
)

// KindSessionChanged is the guest-to-host event a session module emits when
// the console session changes: a login, a logout, a lock or unlock, a fast
// user switch. No correlation id: it answers nothing.
const KindSessionChanged = "weave.session.changed"

// SessionState is what a logged-in session is doing.
type SessionState string

const (
	// SessionActive is in use: unlocked, and in front of the user.
	SessionActive SessionState = "active"
	// SessionLocked is logged in behind a lock screen.
	SessionLocked SessionState = "locked"
	// SessionInactive is logged in but not in front of anyone: switched away
	// from with fast user switching, or a disconnected remote session.
	SessionInactive SessionState = "inactive"
)

// SessionInfo is one logged-in session.
type SessionInfo struct {
	// ID is the OS's own session identifier: a logind session id, a Windows
	// session id, a macOS audit session id.
	ID   string `json:"id"`
	User string `json:"user"`
	// UID is the user's numeric id on Unix and SID on Windows, for a host
	// that must not confuse two accounts with one display name.
	UID   string       `json:"uid,omitempty"`
	State SessionState `json:"state"`
	// Console reports the session at the physical console — the one
	// per-user-console capabilities run in.
	Console bool `json:"console,omitempty"`
	// Remote reports a session reached over the network (RDP, SSH, Screen
	// Sharing as a separate login).
	Remote bool `json:"remote,omitempty"`
	// Since is when the session began, where the OS records it.
	Since time.Time `json:"since,omitzero"`
}

// SessionCurrentResponse reports the console session. Session is nil when
// nobody is logged in at the console — an answer, not a failure: it is
// exactly the state in which clipboard and display do not answer at all.
type SessionCurrentResponse struct {
	Session *SessionInfo `json:"session,omitempty"`
}

// SessionListResponse is every logged-in session the OS can enumerate.
type SessionListResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionLockRequest asks for a session's screen to be locked.
type SessionLockRequest struct {
	// SessionID is the session to lock; empty locks the console session.
	SessionID string `json:"session_id,omitempty"`
}

// SessionLockResponse names the session that was locked, which is the one a
// host must watch for KindSessionChanged when it asked for "the console".
type SessionLockResponse struct {
	SessionID string `json:"session_id"`
}

// SessionChangedEvent is the payload of KindSessionChanged. Either side is
// nil when there was, or now is, nobody at the console.
//
// A host that syncs the clipboard resets its change state on this event: a
// new console session runs a new clipboard module, whose change tokens have
// nothing to do with the last one's.
type SessionChangedEvent struct {
	Previous  *SessionInfo `json:"previous,omitempty"`
	Current   *SessionInfo `json:"current,omitempty"`
	ChangedAt time.Time    `json:"changed_at"`
}
