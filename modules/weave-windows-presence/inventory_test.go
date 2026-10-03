//go:build windows

package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/systeminformation"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Against the real system. The registry reads especially: UTF-16 sizing is
// the kind of thing that looks right and returns a truncated or NUL-padded
// string.
func TestInventoryReadsTheRealSystem(t *testing.T) {
	var inv weavewire.InventoryResponse
	newInventory().Collect(context.Background(), &inv)

	if inv.UptimeSeconds == 0 {
		t.Error("uptime reported as zero")
	}
	if inv.MemoryBytes == 0 {
		t.Error("total physical memory reported as zero")
	}
	if inv.CPUModel == "" {
		t.Error("no CPU model from the registry")
	}
	if inv.OSVersion == "" || !strings.HasSuffix(inv.OSVersion, "."+inv.OSBuild) {
		t.Errorf("version %q, build %q", inv.OSVersion, inv.OSBuild)
	}
	for name, got := range map[string]string{
		"cpu model":      inv.CPUModel,
		"hardware model": inv.HardwareModel,
		"serial number":  inv.SerialNumber,
	} {
		if strings.ContainsRune(got, 0) {
			t.Errorf("%s contains a NUL, so decoding is wrong: %q", name, got)
		}
		if got != strings.TrimSpace(got) {
			t.Errorf("%s was not trimmed: %q", name, got)
		}
	}
	t.Logf("os=%q build=%q cpu=%q model=%q serial=%q mem=%d uptime=%d",
		inv.OSVersion, inv.OSBuild, inv.CPUModel, inv.HardwareModel, inv.SerialNumber,
		inv.MemoryBytes, inv.UptimeSeconds)
}

func fakeRegistry(values map[string]string) func(path, name string) string {
	return func(path, name string) string { return values[path+`\`+name] }
}

// fakeFirmware serves table as the RSMB firmware table, reporting the
// insufficient-buffer error the real sizing call reports.
func fakeFirmware(table []byte) firmwareTableFunc {
	return func(_ systeminformation.FIRMWARE_TABLE_PROVIDER, _ uint32, buf []byte) (uint32, error) {
		if len(buf) < len(table) {
			return uint32(len(table)), errors.New("insufficient buffer")
		}
		return uint32(copy(buf, table)), nil
	}
}

func TestInventoryFromKnownSources(t *testing.T) {
	i := inventory{
		tickCount64: func() uint64 { return 3_600_999 },
		memoryStatus: func(m *systeminformation.MEMORYSTATUSEX) error {
			if m.DwLength == 0 {
				return errors.New("DwLength not set")
			}
			m.UllTotalPhys = 16 << 30
			return nil
		},
		regString: fakeRegistry(map[string]string{
			cpuKey + `\ProcessorNameString`: "AMD EPYC 9V74 80-Core Processor",
			biosKey + `\SystemProductName`:  "Virtual Machine",
		}),
		version:       func() (string, string) { return "10.0.22631", "22631" },
		firmwareTable: fakeFirmware(rawSMBIOS(systemInfo("Microsoft Corporation", "Virtual Machine", "1234-5678-90"))),
	}
	var inv weavewire.InventoryResponse
	i.Collect(context.Background(), &inv)

	want := weavewire.InventoryResponse{
		UptimeSeconds: 3600,
		MemoryBytes:   16 << 30,
		OSVersion:     "10.0.22631",
		OSBuild:       "22631",
		CPUModel:      "AMD EPYC 9V74 80-Core Processor",
		HardwareModel: "Virtual Machine",
		SerialNumber:  "1234-5678-90",
	}
	if !reflect.DeepEqual(inv, want) {
		t.Fatalf("inventory = %+v\nwant %+v", inv, want)
	}
}

func TestInventoryToleratesEverySourceFailing(t *testing.T) {
	i := inventory{
		tickCount64:   func() uint64 { return 999 },
		memoryStatus:  func(*systeminformation.MEMORYSTATUSEX) error { return errors.New("unavailable") },
		regString:     fakeRegistry(nil),
		version:       func() (string, string) { return "", "" },
		firmwareTable: fakeFirmware(nil),
	}
	inv := weavewire.InventoryResponse{Hostname: "kept"}
	i.Collect(context.Background(), &inv)
	if !reflect.DeepEqual(inv, weavewire.InventoryResponse{Hostname: "kept"}) {
		t.Fatalf("inventory = %+v, want only the portable half", inv)
	}
}

func TestRegStringAndDWORD(t *testing.T) {
	if got := regString(`SOFTWARE\NoSuchKeyAnywhere\weave`, "x"); got != "" {
		t.Errorf("missing key read as %q", got)
	}
	if got := regString(cpuKey, "NoSuchValueAnywhere"); got != "" {
		t.Errorf("missing value read as %q", got)
	}
	// ~MHz is a REG_DWORD: RRF_RT_REG_SZ refuses it rather than decoding four
	// bytes of number as text, and regDWORD reads it.
	if got := regString(cpuKey, "~MHz"); got != "" {
		t.Errorf("REG_DWORD read as string %q", got)
	}
	if mhz, ok := regDWORD(cpuKey, "~MHz"); !ok || mhz == 0 {
		t.Errorf("~MHz = %d, %v", mhz, ok)
	}
	if _, ok := regDWORD(cpuKey, "ProcessorNameString"); ok {
		t.Error("REG_SZ read as a DWORD")
	}
}

func TestWindowsVersion(t *testing.T) {
	strs := map[string]string{currentVersionKey + `\CurrentBuildNumber`: "22631"}
	dwords := map[string]uint32{
		currentVersionKey + `\CurrentMajorVersionNumber`: 10,
		currentVersionKey + `\CurrentMinorVersionNumber`: 0,
	}
	str := func(p, n string) string { return strs[p+`\`+n] }
	dword := func(p, n string) (uint32, bool) {
		v, ok := dwords[p+`\`+n]
		return v, ok
	}
	if v, b := windowsVersion(str, dword); v != "10.0.22631" || b != "22631" {
		t.Errorf("windowsVersion = %q, %q", v, b)
	}
	// Before Windows 10 there are no version DWORDs: the build alone is known.
	delete(dwords, currentVersionKey+`\CurrentMinorVersionNumber`)
	if v, b := windowsVersion(str, dword); v != "" || b != "22631" {
		t.Errorf("without a minor version: %q, %q", v, b)
	}
}

// systemInfo builds an SMBIOS type 1 structure: manufacturer, product name,
// version (none) and serial number string fields, then its strings.
func systemInfo(manufacturer, product, serial string) []byte {
	s := []byte{1, 0x1b, 0x01, 0x00, 1, 2, 0, 3}
	s = append(s, make([]byte, 0x1b-len(s))...)
	for _, str := range []string{manufacturer, product, serial} {
		s = append(s, str...)
		s = append(s, 0)
	}
	return append(s, 0)
}

// biosInfo is a type 0 structure with one string, which the search skips.
func biosInfo() []byte {
	s := []byte{0, 0x12, 0x00, 0x00, 1}
	s = append(s, make([]byte, 0x12-len(s))...)
	return append(s, "Hyper-V UEFI Release v4.1\x00\x00"...)
}

var endOfTable = []byte{127, 4, 0xff, 0xff, 0, 0}

// rawSMBIOS prefixes structures with GetSystemFirmwareTable's RawSMBIOSData
// header.
func rawSMBIOS(structs ...[]byte) []byte {
	var table []byte
	for _, s := range structs {
		table = append(table, s...)
	}
	n := len(table)
	raw := []byte{0, 3, 1, 0, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	return append(raw, table...)
}

func TestSMBIOSSystemSerial(t *testing.T) {
	noStrings := []byte{2, 4, 0, 0, 0, 0} // a type 2 with an empty string set
	short := []byte{1, 6, 0, 0, 1, 2, 0, 0}
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"after other structures": {rawSMBIOS(biosInfo(), noStrings, systemInfo("M", "P", " SN-1 "), endOfTable), "SN-1"},
		"no serial string":       {rawSMBIOS(systemInfo("M", "P", "")[:0x1b], []byte{'M', 0, 0}), ""},
		"index past strings":     {rawSMBIOS([]byte{1, 8, 0, 0, 0, 0, 0, 9, 'M', 0, 0}), ""},
		"too short for serial":   {rawSMBIOS(short), ""},
		"end before system":      {rawSMBIOS(biosInfo(), endOfTable, systemInfo("M", "P", "SN")), ""},
		"no system structure":    {rawSMBIOS(biosInfo()), ""},
		"length under header":    {rawSMBIOS([]byte{1, 2, 0, 0, 0, 0}), ""},
		"length past the table":  {rawSMBIOS([]byte{1, 0x40, 0, 0, 0, 0}), ""},
		"unterminated strings":   {rawSMBIOS([]byte{1, 8, 0, 0, 0, 0, 0, 1, 'S', 'N'}), ""},
		"header only":            {[]byte{0, 3, 1, 0}, ""},
		"empty":                  {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := smbiosSystemSerial(tc.raw); got != tc.want {
				t.Fatalf("serial = %q, want %q", got, tc.want)
			}
		})
	}
}

// The header's length bounds the table: bytes past it are not structures.
func TestSMBIOSIgnoresBytesPastTheTableLength(t *testing.T) {
	raw := rawSMBIOS(biosInfo())
	raw = append(raw, systemInfo("M", "P", "SN")...)
	if got := smbiosSystemSerial(raw); got != "" {
		t.Fatalf("serial = %q from past the table", got)
	}
}

func TestReadSMBIOS(t *testing.T) {
	if got := readSMBIOS(fakeFirmware(nil)); got != nil {
		t.Errorf("an empty table read as %v", got)
	}
	overreport := func(_ systeminformation.FIRMWARE_TABLE_PROVIDER, _ uint32, buf []byte) (uint32, error) {
		return uint32(len(buf)) + 1, nil
	}
	if got := readSMBIOS(overreport); got != nil {
		t.Errorf("a table larger than its sizing call read as %v", got)
	}
	table := rawSMBIOS(systemInfo("M", "P", "SN"))
	if got := readSMBIOS(fakeFirmware(table)); !reflect.DeepEqual(got, table) {
		t.Errorf("readSMBIOS = %v, want %v", got, table)
	}
	// The real table: every machine has one, though a VM's serial may be blank.
	if raw := readSMBIOS(systeminformation.GetSystemFirmwareTable); len(raw) <= rawSMBIOSHeader {
		t.Errorf("real SMBIOS table is %d bytes", len(raw))
	}
}
