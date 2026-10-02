//go:build darwin

package main

import (
	"context"
	"fmt"
	"syscall"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/foundation"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// processFacts is what NSProcessInfo answers for the inventory.
type processFacts struct {
	version       foundation.NSOperatingSystemVersion
	memoryBytes   uint64
	uptimeSeconds float64
}

// inventory fills the macOS half of the inventory; weavepresence has already
// collected hostname, OS, arch, cores and the network interfaces.
//
// Foundation answers the OS version, memory and uptime, and IOKit the serial
// number, both through the house binding. The CPU brand, hardware model and
// OS build are sysctls, which the binding does not wrap; they come from the
// standard library's syscall.Sysctl, so the module needs no x/sys either.
//
// The sources are fields so tests can drive the paths a healthy Mac never
// takes.
type inventory struct {
	processInfo func() processFacts
	sysctl      func(name string) (string, error)
	serial      func() string
}

func newInventory() inventory {
	return inventory{processInfo: readProcessInfo, sysctl: syscall.Sysctl, serial: platformSerial}
}

func readProcessInfo() processFacts {
	pi := foundation.NSProcessInfoProcessInfo()
	return processFacts{
		version:       pi.OperatingSystemVersion(),
		memoryBytes:   pi.PhysicalMemory(),
		uptimeSeconds: pi.SystemUptime(),
	}
}

// Collect is best-effort throughout: a field that cannot be read stays empty
// rather than costing the rest of the inventory.
func (i inventory) Collect(_ context.Context, inv *weavewire.InventoryResponse) {
	pf := i.processInfo()
	// A zero major version is NSProcessInfo failing (the framework did not
	// load), not macOS 0.
	if v := pf.version; v.MajorVersion > 0 {
		inv.OSVersion = fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.PatchVersion)
	}
	inv.MemoryBytes = pf.memoryBytes
	if pf.uptimeSeconds >= 1 {
		inv.UptimeSeconds = uint64(pf.uptimeSeconds)
	}

	inv.OSBuild = i.sysctlString("kern.osversion")            // "26A434"
	inv.CPUModel = i.sysctlString("machdep.cpu.brand_string") // "Apple M4"
	inv.HardwareModel = i.sysctlString("hw.model")            // "Mac16,12", "VirtualMac2,1"
	inv.SerialNumber = i.serial()
}

func (i inventory) sysctlString(name string) string {
	v, err := i.sysctl(name)
	if err != nil {
		return ""
	}
	return v
}
