//go:build darwin

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/foundation"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// metrics samples live resource usage on macOS. Mach host statistics (CPU
// ticks and VM pages), Foundation (physical memory) and libproc (the process
// list) come through the house binding. The load average and swap are
// sysctls, which the binding does not wrap, so they come from the standard
// library's syscall.Sysctl, as does statfs(2); the module needs no x/sys.
//
// The sources are fields so tests can drive the paths a healthy Mac never
// takes; newMetrics wires the real ones.
type metrics struct {
	hostStats      func(flavor int32, count uint32) ([]uint32, error)
	physicalMemory func() uint64
	sysctl         func(name string) (string, error)
	statfs         func(path string, buf *syscall.Statfs_t) error
	processCount   func() (int, error)
	pageSize       uint64
	root           string
	cpu            cpuMeter
}

func newMetrics() *metrics {
	return &metrics{
		hostStats:      hostStatistics,
		physicalMemory: func() uint64 { return foundation.NSProcessInfoProcessInfo().PhysicalMemory() },
		sysctl:         syscall.Sysctl,
		statfs:         syscall.Statfs,
		processCount:   processCount,
		pageSize:       uint64(os.Getpagesize()), //nolint:gosec // G115: a page size is positive.
		root:           "/",
	}
}

// Sample fills the macOS half of a metrics reading.
//
// Every field is best-effort: a reading missing swap is still worth returning,
// and a host watching a guest fill its disk should not lose that because the
// CPU counters were unavailable. A field that cannot be read is left zero,
// which the wire omits.
//
// macOS rate-limits host_statistics64 for a caller without Apple's
// entitlement and answers calls inside its window from a cache, so samples
// taken well under a second apart can report 0% CPU for the interval. A host
// sampling at the cadence a graph needs is not affected.
func (m *metrics) Sample(_ context.Context, out *weavewire.MetricsResponse) error {
	if ticks, err := m.hostStats(hostCPULoadInfo, cpuStateMax); err == nil {
		busy := uint64(
			ticks[cpuStateUser],
		) + uint64(
			ticks[cpuStateSystem],
		) + uint64(
			ticks[cpuStateNice],
		)
		out.CPUPercent = m.cpu.percent(busy, busy+uint64(ticks[cpuStateIdle]))
	}
	if raw, err := m.sysctl("vm.loadavg"); err == nil {
		if load, err := parseLoadavg(raw); err == nil {
			out.LoadAverage = load
		}
	}

	out.MemoryTotalBytes = m.physicalMemory()
	// Free pages plus inactive ones, which the kernel reclaims on demand,
	// approximate what a new allocation could get; wired and active pages are
	// spoken for.
	if pages, err := m.hostStats(hostVMInfo64, hostVMInfo64Count); err == nil {
		out.MemoryAvailableBytes = (uint64(pages[vmFreeCount]) + uint64(pages[vmInactiveCount])) * m.pageSize
	}
	if raw, err := m.sysctl("vm.swapusage"); err == nil {
		if total, used, err := parseSwapusage(raw); err == nil {
			out.SwapTotalBytes, out.SwapUsedBytes = total, used
		}
	}

	var fs syscall.Statfs_t
	if err := m.statfs(m.root, &fs); err == nil {
		bsize := uint64(fs.Bsize)
		out.Disks = []weavewire.DiskUsage{{
			Mountpoint: m.root,
			TotalBytes: fs.Blocks * bsize,
			// Bavail, not Bfree: what an unprivileged writer can still use.
			FreeBytes: fs.Bavail * bsize,
		}}
	}
	if n, err := m.processCount(); err == nil {
		out.ProcessCount = n
	}
	return nil
}

var errSysctlFormat = errors.New("unexpected sysctl layout")

// parseLoadavg decodes vm.loadavg, a struct loadavg { fixpt_t ldavg[3]; long
// fscale; }: three uint32 fixed-point values, four bytes of padding and an
// int64 scale, 24 bytes on arm64.
//
// syscall.Sysctl returns the value as a string and drops one trailing NUL
// byte, which for this struct is the scale's most significant byte, always
// zero. Padding back to the full size restores it rather than rejecting a
// value that was read correctly.
func parseLoadavg(raw string) ([]float64, error) {
	const size = 24
	b := []byte(raw)
	if len(b) < size-1 || len(b) > size {
		return nil, fmt.Errorf("%w: vm.loadavg is %d bytes, want %d", errSysctlFormat, len(b), size)
	}
	b = append(b, make([]byte, size-len(b))...)
	scale := binary.LittleEndian.Uint64(b[16:])
	if scale == 0 {
		return nil, fmt.Errorf("%w: vm.loadavg has no scale", errSysctlFormat)
	}
	out := make([]float64, 3)
	for i := range out {
		out[i] = float64(binary.LittleEndian.Uint32(b[i*4:])) / float64(scale)
	}
	return out, nil
}

// parseSwapusage decodes vm.swapusage, a struct xsw_usage { u_int64_t
// xsu_total, xsu_avail, xsu_used; u_int32_t xsu_pagesize; boolean_t
// xsu_encrypted; }: 32 bytes. As with vm.loadavg, a trailing zero byte may
// have been dropped by syscall.Sysctl; xsu_encrypted's high byte is zero.
func parseSwapusage(raw string) (total, used uint64, err error) {
	const size = 32
	b := []byte(raw)
	if len(b) < 24 || len(b) > size {
		return 0, 0, fmt.Errorf(
			"%w: vm.swapusage is %d bytes, want %d",
			errSysctlFormat,
			len(b),
			size,
		)
	}
	return binary.LittleEndian.Uint64(b[0:]), binary.LittleEndian.Uint64(b[16:]), nil
}
