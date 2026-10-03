//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/security"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/threading"
)

// errNotHeld reports a privilege the process's account does not hold, so
// enabling it changed nothing.
var errNotHeld = errors.New("the account running this module does not hold the privilege")

// tokenPrivilegesSize bounds the TOKEN_PRIVILEGES read back: a count and one
// 12-byte entry per privilege, and a token has a few dozen at most, so 4 KiB
// is room to spare.
const tokenPrivilegesSize = 4096

// privileges enables a privilege in this process's token.
//
// An account that holds a privilege such as SeShutdownPrivilege still has it
// disabled until the process enables it, and the call that needs it fails with
// ERROR_ACCESS_DENIED until then. The token operations are fields so tests can
// drive the failures a healthy service never meets.
type privileges struct {
	openToken func(token *foundation.HANDLE) error
	lookup    func(name string, luid *foundation.LUID) error
	adjust    func(token foundation.HANDLE, state *security.TOKEN_PRIVILEGES) error
	query     func(token foundation.HANDLE, buf []byte, n *uint32) error
}

func processPrivileges() privileges {
	return privileges{
		openToken: func(token *foundation.HANDLE) error {
			return threading.OpenProcessToken(threading.GetCurrentProcess(),
				security.TOKEN_ADJUST_PRIVILEGES|security.TOKEN_QUERY, token)
		},
		lookup: func(name string, luid *foundation.LUID) error {
			return security.LookupPrivilegeValue(nil, name, luid)
		},
		adjust: func(token foundation.HANDLE, state *security.TOKEN_PRIVILEGES) error {
			return security.AdjustTokenPrivileges(
				token,
				false,
				state,
				uint32(unsafe.Sizeof(*state)),
				nil,
				nil,
			)
		},
		query: func(token foundation.HANDLE, buf []byte, n *uint32) error {
			return security.GetTokenInformation(token, security.TokenPrivileges, buf, n)
		},
	}
}

// enable turns on one privilege, by name, and confirms it took.
//
// AdjustTokenPrivileges succeeds even when the account does not hold the
// privilege: it reports that only as ERROR_NOT_ALL_ASSIGNED in the thread's
// last error. That cannot be read afterwards through the binding — the Go
// runtime clears the last error before every system call, GetLastError's
// own included — so the token is read back instead, which answers the
// question directly.
func (p privileges) enable(name string) error {
	var token foundation.HANDLE
	if err := p.openToken(&token); err != nil {
		return fmt.Errorf("opening the process token: %w", err)
	}
	defer func() { _ = foundation.CloseHandle(token) }()

	var luid foundation.LUID
	if err := p.lookup(name, &luid); err != nil {
		return fmt.Errorf("looking up %s: %w", name, err)
	}
	state := security.TOKEN_PRIVILEGES{
		PrivilegeCount: 1,
		Privileges: [1]security.LUID_AND_ATTRIBUTES{{
			Luid:       luid,
			Attributes: security.SE_PRIVILEGE_ENABLED,
		}},
	}
	if err := p.adjust(token, &state); err != nil {
		return fmt.Errorf("enabling %s: %w", name, err)
	}

	buf := make([]byte, tokenPrivilegesSize)
	n := uint32(tokenPrivilegesSize)
	if err := p.query(token, buf, &n); err != nil {
		return fmt.Errorf("reading back %s: %w", name, err)
	}
	if !privilegeEnabled(buf[:min(int(n), len(buf))], luid) {
		return fmt.Errorf("%s: %w", name, errNotHeld)
	}
	return nil
}

// privilegeEnabled reports whether a TOKEN_PRIVILEGES buffer lists luid as
// enabled: a DWORD count, then that many LUID_AND_ATTRIBUTES (LowPart DWORD,
// HighPart LONG, Attributes DWORD). A privilege the account does not hold is
// absent from the list altogether.
func privilegeEnabled(buf []byte, luid foundation.LUID) bool {
	const entry = 12
	if len(buf) < 4 {
		return false
	}
	// The LUID as the token stores it. Encoding a fixed-size struct into a
	// buffer of its size cannot fail.
	want := make([]byte, binary.Size(luid))
	_, _ = binary.Encode(want, binary.LittleEndian, luid)
	count := int(binary.LittleEndian.Uint32(buf))
	for i := range count {
		off := 4 + i*entry
		if off+entry > len(buf) {
			return false
		}
		if bytes.Equal(buf[off:off+len(want)], want) {
			attrs := security.TOKEN_PRIVILEGES_ATTRIBUTES(binary.LittleEndian.Uint32(buf[off+8:]))
			return attrs&security.SE_PRIVILEGE_ENABLED != 0
		}
	}
	return false
}
