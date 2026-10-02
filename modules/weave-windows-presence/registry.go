//go:build windows

package main

import (
	"strings"
	"syscall"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/registry"
)

// maxRegString bounds the buffer regString allocates. The values it reads are
// names, a few dozen bytes; anything near this is not one of them.
const maxRegString = 64 << 10

// regString reads one REG_SZ value under HKEY_LOCAL_MACHINE, returning "" for
// any failure: a key missing on an unusual SKU is not worth an error.
//
// RegGetValue with RRF_RT_REG_SZ is used rather than RegQueryValueEx because it
// rejects other value types and guarantees the terminating NUL, which
// RegQueryValueEx leaves to whoever wrote the value.
func regString(path, name string) string {
	var size uint32
	if rc := registry.RegGetValue(registry.HKEY_LOCAL_MACHINE, &path, &name,
		registry.RRF_RT_REG_SZ, nil, nil, &size); rc != 0 || size == 0 || size > maxRegString {
		return ""
	}
	// size is in bytes and includes the NUL; one spare uint16 absorbs an odd
	// byte count rather than dropping the last character.
	size = (size/2 + 1) * 2
	buf := make([]uint16, size/2)
	if rc := registry.RegGetValue(registry.HKEY_LOCAL_MACHINE, &path, &name,
		registry.RRF_RT_REG_SZ, nil, unsafe.Pointer(&buf[0]), &size); rc != 0 {
		return ""
	}
	// Trimmed because the registry does not promise a tidy value, and at least
	// one real CPU name is not: an Azure AMD EPYC reports its
	// ProcessorNameString followed by sixteen spaces. Untrimmed, it reads as a
	// different processor to anything comparing two machines' inventories.
	return strings.TrimSpace(syscall.UTF16ToString(buf))
}

// regDWORD reads one REG_DWORD value under HKEY_LOCAL_MACHINE, reporting
// whether there was one.
func regDWORD(path, name string) (uint32, bool) {
	var v uint32
	size := uint32(unsafe.Sizeof(v))
	rc := registry.RegGetValue(registry.HKEY_LOCAL_MACHINE, &path, &name,
		registry.RRF_RT_REG_DWORD, nil, unsafe.Pointer(&v), &size)
	return v, rc == 0
}
