//go:build linux

package main

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Against the real system, because a procfs path that moved or a sysinfo
// struct that changed shape only shows up here.
func TestInventoryReadsTheRealSystem(t *testing.T) {
	var inv weavewire.InventoryResponse
	newInventory().Collect(context.Background(), &inv)

	if inv.MemoryBytes == 0 {
		t.Error("total memory reported as zero")
	}
	if inv.UptimeSeconds == 0 {
		t.Error("uptime reported as zero")
	}
	if inv.OSVersion == "" || inv.OSBuild == "" {
		t.Errorf("uname gave release %q, version %q", inv.OSVersion, inv.OSBuild)
	}
	// CPU model, hardware model and serial come from files a container, an
	// ARM board or an unprivileged process may not be able to read, so empty
	// is allowed. A trailing newline is not: it reaches an operator's listing.
	for name, got := range map[string]string{
		"cpu model":      inv.CPUModel,
		"hardware model": inv.HardwareModel,
		"serial number":  inv.SerialNumber,
	} {
		if got != strings.TrimSpace(got) {
			t.Errorf("%s was not trimmed: %q", name, got)
		}
	}
	t.Logf("kernel=%q cpu=%q model=%q serial=%q mem=%d uptime=%d",
		inv.OSVersion, inv.CPUModel, inv.HardwareModel, inv.SerialNumber,
		inv.MemoryBytes, inv.UptimeSeconds)
}

func utsField(dst []byte, s string) { copy(dst, s) }

func TestInventoryFromKnownSources(t *testing.T) {
	files := map[string]string{
		cpuinfoPath:       "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) CPU @ 2.20GHz\n",
		productNamePath:   "Standard PC (Q35 + ICH9, 2009)\n",
		productSerialPath: "  VM-1234\n",
	}
	i := inventory{
		sysinfo: func(si *unix.Sysinfo_t) error {
			si.Totalram = 4096
			si.Unit = 1024
			si.Uptime = 3600
			return nil
		},
		uname: func(u *unix.Utsname) error {
			utsField(u.Release[:], "6.8.0-40-generic")
			utsField(u.Version[:], "#40-Ubuntu SMP PREEMPT_DYNAMIC")
			return nil
		},
		readFile: func(p string) ([]byte, error) {
			if s, ok := files[p]; ok {
				return []byte(s), nil
			}
			return nil, fs.ErrNotExist
		},
	}
	var inv weavewire.InventoryResponse
	i.Collect(context.Background(), &inv)

	want := weavewire.InventoryResponse{
		MemoryBytes:   4096 * 1024,
		UptimeSeconds: 3600,
		OSVersion:     "6.8.0-40-generic",
		OSBuild:       "#40-Ubuntu SMP PREEMPT_DYNAMIC",
		CPUModel:      "Intel(R) Xeon(R) CPU @ 2.20GHz",
		HardwareModel: "Standard PC (Q35 + ICH9, 2009)",
		SerialNumber:  "VM-1234",
	}
	if inv.MemoryBytes != want.MemoryBytes || inv.UptimeSeconds != want.UptimeSeconds ||
		inv.OSVersion != want.OSVersion || inv.OSBuild != want.OSBuild ||
		inv.CPUModel != want.CPUModel || inv.HardwareModel != want.HardwareModel ||
		inv.SerialNumber != want.SerialNumber {
		t.Fatalf("inventory = %+v\nwant %+v", inv, want)
	}
}

// Every source failing leaves every field empty; nothing panics and nothing
// is invented.
func TestInventoryToleratesEverySourceFailing(t *testing.T) {
	errNo := errors.New("unavailable")
	i := inventory{
		sysinfo:  func(*unix.Sysinfo_t) error { return errNo },
		uname:    func(*unix.Utsname) error { return errNo },
		readFile: func(string) ([]byte, error) { return nil, errNo },
	}
	inv := weavewire.InventoryResponse{Hostname: "kept"}
	i.Collect(context.Background(), &inv)
	if !reflect.DeepEqual(inv, weavewire.InventoryResponse{Hostname: "kept"}) {
		t.Fatalf("inventory = %+v, want only the portable half untouched", inv)
	}
}

// A zero uptime (a clock that has not ticked yet, or a sysinfo that reported
// none) stays absent rather than being reported as a fact.
func TestInventoryOmitsAZeroUptime(t *testing.T) {
	i := newInventory()
	i.sysinfo = func(si *unix.Sysinfo_t) error {
		si.Totalram, si.Unit = 1, 1
		return nil
	}
	var inv weavewire.InventoryResponse
	i.Collect(context.Background(), &inv)
	if inv.UptimeSeconds != 0 || inv.MemoryBytes != 1 {
		t.Fatalf("uptime %d memory %d", inv.UptimeSeconds, inv.MemoryBytes)
	}
}

func TestCPUModel(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"x86":         {"processor\t: 0\nmodel name\t: AMD EPYC 7B13\n", "AMD EPYC 7B13"},
		"arm32 style": {"Processor\t: ARMv7 Processor rev 4 (v7l)\n", "ARMv7 Processor rev 4 (v7l)"},
		"arm64 bare":  {"processor\t: 0\nBogoMIPS\t: 48.00\nCPU part\t: 0xd0c\n", ""},
		"first wins":  {"model name : one\nmodel name : two\n", "one"},
		"no colon":    {"garbage line\n", ""},
		"empty":       {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cpuModel([]byte(tc.in)); got != tc.want {
				t.Fatalf("cpuModel = %q, want %q", got, tc.want)
			}
		})
	}
}
