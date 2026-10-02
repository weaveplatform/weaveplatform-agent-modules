//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/security"
)

// Against the real token. Enabling a privilege in the test's own process
// changes nothing outside it. The runner's account may or may not hold the
// privilege; either answer is a real one, and anything else is a broken call.
func TestEnableAgainstTheRealToken(t *testing.T) {
	err := processPrivileges().enable(wantPrivilege)
	if err != nil && !errors.Is(err, errNotHeld) {
		t.Fatalf("enable(%s) = %v", wantPrivilege, err)
	}
	t.Logf("enable(%s) = %v", wantPrivilege, err)

	if err := processPrivileges().enable("SeNoSuchPrivilege"); err == nil ||
		errors.Is(err, errNotHeld) {
		t.Errorf("enable of an unknown privilege = %v, want a lookup failure", err)
	}
}

// stubbed is the real token with one operation replaced.
func stubbed(edit func(*privileges)) privileges {
	p := processPrivileges()
	edit(&p)
	return p
}

func TestEnableReportsEachFailure(t *testing.T) {
	errStep := errors.New("step failed")
	for name, p := range map[string]privileges{
		"open":   stubbed(func(p *privileges) { p.openToken = func(*foundation.HANDLE) error { return errStep } }),
		"lookup": stubbed(func(p *privileges) { p.lookup = func(string, *foundation.LUID) error { return errStep } }),
		"adjust": stubbed(func(p *privileges) {
			p.adjust = func(foundation.HANDLE, *security.TOKEN_PRIVILEGES) error { return errStep }
		}),
		"query": stubbed(func(p *privileges) {
			p.query = func(foundation.HANDLE, []byte, *uint32) error { return errStep }
		}),
	} {
		if err := p.enable(wantPrivilege); !errors.Is(err, errStep) {
			t.Errorf("%s: err = %v, want the step's error", name, err)
		}
	}
}

// AdjustTokenPrivileges succeeds for a privilege the account lacks; only the
// read-back shows it.
func TestEnableReportsAPrivilegeNotHeld(t *testing.T) {
	p := stubbed(func(p *privileges) {
		p.adjust = func(foundation.HANDLE, *security.TOKEN_PRIVILEGES) error { return nil }
		p.query = func(_ foundation.HANDLE, buf []byte, n *uint32) error {
			*n = 4 // a token with no privileges at all
			binary.LittleEndian.PutUint32(buf, 0)
			return nil
		}
	})
	if err := p.enable(wantPrivilege); !errors.Is(err, errNotHeld) {
		t.Fatalf("err = %v, want errNotHeld", err)
	}
}

func TestEnableSucceedsWhenTheTokenShowsItEnabled(t *testing.T) {
	luid := foundation.LUID{LowPart: 19, HighPart: 0}
	p := privileges{
		openToken: func(*foundation.HANDLE) error { return nil },
		lookup:    func(_ string, l *foundation.LUID) error { *l = luid; return nil },
		adjust:    func(foundation.HANDLE, *security.TOKEN_PRIVILEGES) error { return nil },
		query: func(_ foundation.HANDLE, buf []byte, n *uint32) error {
			*n = uint32(copy(buf, tokenPrivileges(
				entry{foundation.LUID{LowPart: 5}, security.SE_PRIVILEGE_ENABLED},
				entry{luid, security.SE_PRIVILEGE_ENABLED},
			)))
			return nil
		},
	}
	if err := p.enable(wantPrivilege); err != nil {
		t.Fatal(err)
	}
}

type entry struct {
	luid  foundation.LUID
	attrs security.TOKEN_PRIVILEGES_ATTRIBUTES
}

func tokenPrivileges(entries ...entry) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(entries)))
	for _, e := range entries {
		b = binary.LittleEndian.AppendUint32(b, e.luid.LowPart)
		b = binary.LittleEndian.AppendUint32(b, uint32(e.luid.HighPart))
		b = binary.LittleEndian.AppendUint32(b, uint32(e.attrs))
	}
	return b
}

func TestPrivilegeEnabled(t *testing.T) {
	target := foundation.LUID{LowPart: 19, HighPart: 1}
	held := tokenPrivileges(
		entry{foundation.LUID{LowPart: 19}, security.SE_PRIVILEGE_ENABLED},
		entry{target, security.SE_PRIVILEGE_ENABLED},
	)
	disabled := tokenPrivileges(entry{target, 0})
	for name, tc := range map[string]struct {
		buf  []byte
		want bool
	}{
		"enabled":   {held, true},
		"disabled":  {disabled, false},
		"absent":    {tokenPrivileges(entry{foundation.LUID{LowPart: 7}, security.SE_PRIVILEGE_ENABLED}), false},
		"truncated": {held[:len(held)-1], false},
		"empty":     {nil, false},
	} {
		if got := privilegeEnabled(tc.buf, target); got != tc.want {
			t.Errorf("%s: privilegeEnabled = %v, want %v", name, got, tc.want)
		}
	}
}
