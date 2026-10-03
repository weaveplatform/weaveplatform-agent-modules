//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"strings"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/systeminformation"
)

// firmwareTableFunc is systeminformation.GetSystemFirmwareTable's shape.
type firmwareTableFunc func(
	provider systeminformation.FIRMWARE_TABLE_PROVIDER,
	id uint32,
	buf []byte,
) (uint32, error)

// readSMBIOS returns the raw SMBIOS table ('RSMB'), or nil.
//
// Success is judged by the size returned, not the error: the binding reports
// the thread's last error, which the sizing call sets to
// ERROR_INSUFFICIENT_BUFFER by design, and which can be stale after a call
// that succeeded.
func readSMBIOS(get firmwareTableFunc) []byte {
	n, _ := get(systeminformation.RSMB, 0, nil)
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	got, _ := get(systeminformation.RSMB, 0, buf)
	if got == 0 || got > n {
		return nil
	}
	return buf[:got]
}

// SMBIOS layout: GetSystemFirmwareTable prefixes the structure table with an
// 8-byte RawSMBIOSData header whose last four bytes are the table's length.
// Each structure is a formatted area (type, length, handle, fields) followed
// by its strings, NUL-separated and ended by an extra NUL. A string field holds
// a 1-based index into those strings; 0 means none.
const (
	rawSMBIOSHeader   = 8
	smbiosTypeSystem  = 1 // System Information
	smbiosTypeEnd     = 127
	systemSerialField = 0x07
)

// smbiosSystemSerial returns the System Information structure's serial
// number, the value Win32_BIOS.SerialNumber reports, or "" when the table has
// none or does not parse.
func smbiosSystemSerial(raw []byte) string {
	if len(raw) < rawSMBIOSHeader {
		return ""
	}
	tableLen := binary.LittleEndian.Uint32(raw[4:8])
	table := raw[rawSMBIOSHeader:]
	if uint64(tableLen) < uint64(len(table)) {
		table = table[:tableLen]
	}
	for len(table) >= 4 {
		typ, length := table[0], int(table[1])
		if length < 4 || length > len(table) {
			return ""
		}
		// The strings end at the first double NUL after the formatted area.
		end := bytes.Index(table[length:], []byte{0, 0})
		if end < 0 {
			return ""
		}
		strs := table[length : length+end]
		if typ == smbiosTypeSystem {
			if length <= systemSerialField {
				return ""
			}
			return smbiosString(strs, table[systemSerialField])
		}
		if typ == smbiosTypeEnd {
			return ""
		}
		table = table[length+end+2:]
	}
	return ""
}

// smbiosString returns the index'th (1-based) string of a structure.
func smbiosString(strs []byte, index byte) string {
	if index == 0 {
		return ""
	}
	parts := bytes.Split(strs, []byte{0})
	if int(index) > len(parts) {
		return ""
	}
	return strings.TrimSpace(string(parts[index-1]))
}
