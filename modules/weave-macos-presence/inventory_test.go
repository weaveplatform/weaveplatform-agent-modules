//go:build darwin

package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/foundation"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Against the real system: the only way to learn that a binding call returns
// something usable rather than a zero value.
func TestInventoryReadsTheRealSystem(t *testing.T) {
	var inv weavewire.InventoryResponse
	newInventory().Collect(context.Background(), &inv)

	if inv.OSVersion == "" || strings.Count(inv.OSVersion, ".") != 2 {
		t.Errorf("OS version %q, want major.minor.patch from NSProcessInfo", inv.OSVersion)
	}
	if inv.MemoryBytes == 0 {
		t.Error("physical memory reported as zero")
	}
	if inv.UptimeSeconds == 0 {
		t.Error("uptime reported as zero")
	}
	if inv.OSBuild == "" {
		t.Error("no OS build from kern.osversion")
	}
	if inv.CPUModel == "" {
		t.Error("no CPU model from machdep.cpu.brand_string")
	}
	if inv.HardwareModel == "" {
		t.Error("no hardware model from hw.model")
	}
	// Some virtual machines have no serial; a value, when there is one, must
	// be the serial and not a description of the object holding it.
	if s := inv.SerialNumber; len(s) > 64 || strings.ContainsAny(s, "<>{}\n") {
		t.Errorf("serial number looks like an object description: %q", s)
	}
	t.Logf("os=%q build=%q cpu=%q model=%q serial=%q mem=%d uptime=%d",
		inv.OSVersion, inv.OSBuild, inv.CPUModel, inv.HardwareModel, inv.SerialNumber,
		inv.MemoryBytes, inv.UptimeSeconds)
}

func TestInventoryFromKnownSources(t *testing.T) {
	sysctls := map[string]string{
		"kern.osversion":           "26A434",
		"machdep.cpu.brand_string": "Apple M4",
		"hw.model":                 "VirtualMac2,1",
	}
	i := inventory{
		processInfo: func() processFacts {
			return processFacts{
				version: foundation.NSOperatingSystemVersion{
					MajorVersion: 27, MinorVersion: 0, PatchVersion: 1,
				},
				memoryBytes:   8 << 30,
				uptimeSeconds: 3600.75,
			}
		},
		sysctl: func(name string) (string, error) {
			if v, ok := sysctls[name]; ok {
				return v, nil
			}
			return "", errors.New("no such sysctl")
		},
		serial: func() string { return "ZXCV1234" },
	}
	var inv weavewire.InventoryResponse
	i.Collect(context.Background(), &inv)

	want := weavewire.InventoryResponse{
		OSVersion:     "27.0.1",
		OSBuild:       "26A434",
		MemoryBytes:   8 << 30,
		UptimeSeconds: 3600,
		CPUModel:      "Apple M4",
		HardwareModel: "VirtualMac2,1",
		SerialNumber:  "ZXCV1234",
	}
	if !reflect.DeepEqual(inv, want) {
		t.Fatalf("inventory = %+v\nwant %+v", inv, want)
	}
}

// Foundation not loading, every sysctl failing and no serial leave every field
// empty; nothing is invented, and the portable half is untouched.
func TestInventoryToleratesEverySourceFailing(t *testing.T) {
	i := inventory{
		processInfo: func() processFacts { return processFacts{uptimeSeconds: 0.5} },
		sysctl:      func(string) (string, error) { return "", errors.New("unavailable") },
		serial:      func() string { return "" },
	}
	inv := weavewire.InventoryResponse{Hostname: "kept"}
	i.Collect(context.Background(), &inv)
	if !reflect.DeepEqual(inv, weavewire.InventoryResponse{Hostname: "kept"}) {
		t.Fatalf("inventory = %+v, want only the portable half", inv)
	}
}

// A path with no registry entry, and a property that is not a string, both
// read as absent rather than as garbage.
func TestRegistryStringAbsentAndNonString(t *testing.T) {
	if got := registryString("IOService:/NoSuchEntryAnywhere", "IOPlatformSerialNumber"); got != "" {
		t.Errorf("missing entry read as %q", got)
	}
	if got := registryString("IOService:/", "NoSuchPropertyAnywhere"); got != "" {
		t.Errorf("missing property read as %q", got)
	}
	// IOPlatformUUID is a string on every Mac; "model" is CFData.
	if got := registryString("IOService:/", "IOPlatformUUID"); len(got) != 36 {
		t.Errorf("IOPlatformUUID = %q, want a UUID", got)
	}
	if got := registryString("IOService:/", "model"); got != "" {
		t.Errorf("CFData property read as string %q", got)
	}
}
