//go:build windows

package main

import (
	"fmt"
	"runtime"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/security"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/security/authorization"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/remotedesktop"
)

// querySession copies one WTSQuerySessionInformation answer out of the buffer
// Terminal Services allocated, and frees it.
func querySession(id uint32, class remotedesktop.WTS_INFO_CLASS) ([]byte, error) {
	var buf foundation.PWSTR
	var n uint32
	if err := remotedesktop.WTSQuerySessionInformation(
		remotedesktop.WTS_CURRENT_SERVER_HANDLE, id, class, &buf, &n,
	); err != nil {
		return nil, fmt.Errorf("WTSQuerySessionInformation(%d, %d): %w", id, class, err)
	}
	defer remotedesktop.WTSFreeMemory(unsafe.Pointer(buf))
	if buf == nil || n == 0 {
		return nil, nil
	}
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(buf)), n)...), nil
}

// enumerateSessions lists every session id, services' session 0 included.
func enumerateSessions() ([]uint32, error) {
	var p *remotedesktop.WTS_SESSION_INFOW
	var count uint32
	if err := remotedesktop.WTSEnumerateSessions(
		remotedesktop.WTS_CURRENT_SERVER_HANDLE, 0, 1, &p, &count,
	); err != nil {
		return nil, fmt.Errorf("WTSEnumerateSessions: %w", err)
	}
	defer remotedesktop.WTSFreeMemory(unsafe.Pointer(p))
	ids := make([]uint32, 0, count)
	for _, s := range unsafe.Slice(p, count) {
		ids = append(ids, s.SessionId)
	}
	return ids, nil
}

// Buffer sizes for LookupAccountNameW: SECURITY_MAX_SID_SIZE, and a domain
// name well beyond DNLEN, so one call answers and no size probe is needed.
const (
	maxSIDBytes   = 68
	maxDomainName = 256
)

// accountSID resolves DOMAIN\user to its SID string (S-1-5-21-…), the
// identity a host can tell two same-named accounts apart by.
func accountSID(account string) (string, error) {
	sid := make([]byte, maxSIDBytes)
	sidLen := uint32(maxSIDBytes)
	domain := make([]uint16, maxDomainName)
	domainLen := uint32(maxDomainName)
	var use security.SID_NAME_USE
	err := security.LookupAccountName(nil, account,
		security.PSID(unsafe.Pointer(&sid[0])), &sidLen, &domain[0], &domainLen, &use)
	if err != nil {
		return "", fmt.Errorf("LookupAccountName(%s): %w", account, err)
	}
	var str foundation.PWSTR
	err = authorization.ConvertSidToStringSid(security.PSID(unsafe.Pointer(&sid[0])), &str)
	runtime.KeepAlive(sid)
	if err != nil {
		return "", fmt.Errorf("ConvertSidToStringSid(%s): %w", account, err)
	}
	defer func() { _, _ = foundation.LocalFree(foundation.HLOCAL(unsafe.Pointer(str))) }()
	n := 0
	for p := unsafe.Pointer(str); *(*uint16)(p) != 0; p = unsafe.Add(p, 2) {
		n++
	}
	return utf16String(unsafe.Slice((*byte)(unsafe.Pointer(str)), 2*n)), nil
}
