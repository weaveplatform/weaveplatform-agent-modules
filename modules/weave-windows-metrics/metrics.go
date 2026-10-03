//go:build windows

package main

import (
	"context"
	"os"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/storage/filesystem"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/processstatus"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/systeminformation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/threading"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// metrics samples live resource usage through the house binding:
// GetSystemTimes for CPU, GlobalMemoryStatusEx for memory and the page file,
// GetPerformanceInfo for the process count and GetDiskFreeSpaceEx for the
// system volume. None of them needs a privilege.
//
// GetSystemTimes is used rather than PDH: PDH means opening a query, adding
// counters by a path string that is localised to the system's language, and
// collecting twice before the first number appears, for what is three
// FILETIME counters here. WMI is avoided for the same reason, and because it
// needs a COM apartment.
//
// The sources are fields so tests can drive the paths a healthy system never
// takes; newMetrics wires the real ones.
type metrics struct {
	systemTimes  func(idle, kernel, user *foundation.FILETIME) error
	memoryStatus func(*systeminformation.MEMORYSTATUSEX) error
	performance  func(*processstatus.PERFORMANCE_INFORMATION) error
	diskFree     func(volume string, freeToCaller, total, free *uint64) error
	volume       string
	cpu          cpuMeter
}

func newMetrics() *metrics {
	return &metrics{
		systemTimes:  threading.GetSystemTimes,
		memoryStatus: systeminformation.GlobalMemoryStatusEx,
		performance: func(p *processstatus.PERFORMANCE_INFORMATION) error {
			return processstatus.GetPerformanceInfo(p, p.Cb)
		},
		diskFree: func(volume string, freeToCaller, total, free *uint64) error {
			return filesystem.GetDiskFreeSpaceEx(&volume, freeToCaller, total, free)
		},
		volume: systemVolume(os.Getenv("SystemDrive")),
	}
}

// systemVolume is the root of the volume Windows runs from. SystemDrive is
// set for every process, services included; C: is only its usual value.
func systemVolume(drive string) string {
	if drive == "" {
		drive = "C:"
	}
	return drive + `\`
}

// Sample fills the Windows half of a metrics reading.
//
// Every field is best-effort: a host watching a guest fill its disk should not
// lose that because another counter was unavailable. A field that cannot be
// read is left zero, which the wire omits. LoadAverage is always left empty:
// Windows has no equivalent, and one invented from CPU utilisation would be a
// different measurement wearing its name.
func (m *metrics) Sample(_ context.Context, out *weavewire.MetricsResponse) error {
	var idleTime, kernelTime, userTime foundation.FILETIME
	if err := m.systemTimes(&idleTime, &kernelTime, &userTime); err == nil {
		// Kernel time includes idle time, so kernel + user is the total and
		// busy is what is left of it after idle.
		total := ticks(kernelTime) + ticks(userTime)
		out.CPUPercent = m.cpu.percent(total-ticks(idleTime), total)
	}

	mem := systeminformation.MEMORYSTATUSEX{
		DwLength: uint32(unsafe.Sizeof(systeminformation.MEMORYSTATUSEX{})),
	}
	if err := m.memoryStatus(&mem); err == nil {
		out.MemoryTotalBytes = mem.UllTotalPhys
		out.MemoryAvailableBytes = mem.UllAvailPhys
		out.SwapTotalBytes, out.SwapUsedBytes = pageFile(mem)
	}

	perf := processstatus.PERFORMANCE_INFORMATION{
		Cb: uint32(unsafe.Sizeof(processstatus.PERFORMANCE_INFORMATION{})),
	}
	if err := m.performance(&perf); err == nil {
		out.ProcessCount = int(perf.ProcessCount)
	}

	var freeToCaller, total, free uint64
	if err := m.diskFree(m.volume, &freeToCaller, &total, &free); err == nil {
		out.Disks = []weavewire.DiskUsage{{
			Mountpoint: m.volume,
			TotalBytes: total,
			// The quota-aware figure rather than the raw free space: on a
			// volume with quotas it is what can actually still be written.
			FreeBytes: freeToCaller,
		}}
	}
	return nil
}

// pageFile derives the swap figures from the commit totals.
// GlobalMemoryStatusEx's "page file" fields are the commit limit and what is
// left of it, and both include physical memory, so the page file alone is the
// difference. Reporting the raw total would show a machine with no page file
// as having as much swap as it has RAM.
func pageFile(mem systeminformation.MEMORYSTATUSEX) (total, used uint64) {
	if mem.UllTotalPageFile <= mem.UllTotalPhys || mem.UllAvailPageFile > mem.UllTotalPageFile {
		return 0, 0
	}
	total = mem.UllTotalPageFile - mem.UllTotalPhys
	committed := mem.UllTotalPageFile - mem.UllAvailPageFile
	physUsed := mem.UllTotalPhys - min(mem.UllAvailPhys, mem.UllTotalPhys)
	if committed > physUsed {
		used = min(committed-physUsed, total)
	}
	return total, used
}

// ticks joins a FILETIME's halves into one count of 100ns intervals.
func ticks(ft foundation.FILETIME) uint64 {
	return uint64(ft.DwHighDateTime)<<32 | uint64(ft.DwLowDateTime)
}
