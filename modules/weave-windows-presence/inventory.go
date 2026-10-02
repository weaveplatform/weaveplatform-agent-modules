//go:build windows

package main

import (
	"context"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/systeminformation"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

const (
	cpuKey  = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`
	biosKey = `HARDWARE\DESCRIPTION\System\BIOS`
)

// inventory fills the Windows half of the inventory; weavepresence has already
// collected hostname, OS, arch, cores and the network interfaces.
//
// Everything goes through the house Win32 binding: uptime, memory and the
// SMBIOS serial number through system/systeminformation, and the OS version,
// CPU and system model through system/registry. SystemProductName is the
// SMBIOS System Information product name, the same value
// Win32_ComputerSystem.Model reports ("Virtual Machine" on Hyper-V).
//
// The sources are fields so tests can drive the paths a healthy machine never
// takes.
type inventory struct {
	tickCount64   func() uint64
	memoryStatus  func(*systeminformation.MEMORYSTATUSEX) error
	regString     func(path, name string) string
	version       func() (version, build string)
	firmwareTable firmwareTableFunc
}

func newInventory() inventory {
	return inventory{
		tickCount64:   systeminformation.GetTickCount64,
		memoryStatus:  systeminformation.GlobalMemoryStatusEx,
		regString:     regString,
		version:       func() (string, string) { return windowsVersion(regString, regDWORD) },
		firmwareTable: systeminformation.GetSystemFirmwareTable,
	}
}

// Collect is best-effort throughout: a field that cannot be read stays empty
// rather than costing the rest of the inventory.
func (i inventory) Collect(_ context.Context, inv *weavewire.InventoryResponse) {
	// GetTickCount64 counts milliseconds since boot and, unlike GetTickCount,
	// does not wrap after 49.7 days, which a long-lived VM reaches.
	inv.UptimeSeconds = i.tickCount64() / 1000

	var mem systeminformation.MEMORYSTATUSEX
	mem.DwLength = uint32(unsafe.Sizeof(mem))
	if err := i.memoryStatus(&mem); err == nil {
		inv.MemoryBytes = mem.UllTotalPhys
	}

	inv.OSVersion, inv.OSBuild = i.version()
	inv.CPUModel = i.regString(cpuKey, "ProcessorNameString")
	inv.HardwareModel = i.regString(biosKey, "SystemProductName")
	inv.SerialNumber = smbiosSystemSerial(readSMBIOS(i.firmwareTable))
}
