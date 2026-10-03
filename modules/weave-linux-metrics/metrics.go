//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// loadScale is the fixed-point scale of sysinfo(2)'s load averages
// (1 << SI_LOAD_SHIFT).
const loadScale = 1 << 16

// metrics samples live resource usage from the kernel: sysinfo(2) for memory,
// swap, load and the process count, /proc for CPU time and available memory,
// and statfs(2) for the root filesystem.
//
// The sources are fields so tests can drive the paths a healthy system never
// takes; newMetrics wires the real ones.
type metrics struct {
	proc    string
	root    string
	sysinfo func(*unix.Sysinfo_t) error
	statfs  func(string, *unix.Statfs_t) error
	cpu     cpuMeter
}

func newMetrics() *metrics {
	return &metrics{proc: "/proc", root: "/", sysinfo: unix.Sysinfo, statfs: unix.Statfs}
}

// Sample fills the Linux half of a metrics reading.
//
// Every field is best-effort: a container without some of /proc should still
// report memory, and a host watching a guest fill its disk should not lose that
// because the CPU counters were unreadable. A field that cannot be read is
// left zero, which the wire omits.
func (m *metrics) Sample(_ context.Context, out *weavewire.MetricsResponse) error {
	if busy, total, err := cpuTotals(filepath.Join(m.proc, "stat")); err == nil {
		out.CPUPercent = m.cpu.percent(busy, total)
	}

	var info unix.Sysinfo_t
	if err := m.sysinfo(&info); err == nil {
		unit := uint64(info.Unit)
		out.MemoryTotalBytes = uint64(info.Totalram) * unit
		out.SwapTotalBytes = uint64(info.Totalswap) * unit
		out.SwapUsedBytes = (uint64(info.Totalswap) - uint64(info.Freeswap)) * unit
		out.ProcessCount = int(info.Procs)
		out.LoadAverage = []float64{
			float64(info.Loads[0]) / loadScale,
			float64(info.Loads[1]) / loadScale,
			float64(info.Loads[2]) / loadScale,
		}
	}

	// MemAvailable rather than sysinfo's freeram: the kernel's own estimate of
	// what a new allocation could get, counting reclaimable cache. Free memory
	// alone reads near zero on any warm system and would look like an
	// emergency on a healthy one.
	if available, err := meminfoField(
		filepath.Join(m.proc, "meminfo"),
		"MemAvailable",
	); err == nil {
		out.MemoryAvailableBytes = available
	}

	var fs unix.Statfs_t
	if err := m.statfs(m.root, &fs); err == nil {
		bsize := uint64(fs.Bsize) //nolint:gosec // G115: a block size is never negative.
		out.Disks = []weavewire.DiskUsage{{
			Mountpoint: m.root,
			TotalBytes: fs.Blocks * bsize,
			// Bavail, not Bfree: what an unprivileged writer can still use,
			// without the blocks reserved for root.
			FreeBytes: fs.Bavail * bsize,
		}}
	}
	return nil
}

// cpuTotals reads the aggregate "cpu" line of /proc/stat.
//
// Idle and iowait are the time not spent working; user, nice, system, irq,
// softirq and steal are busy. Older kernels stop before steal, so whatever of
// those eight is present is summed. guest and guest_nice, after them, are
// already counted inside user and nice, and adding them again would inflate
// both sides on a machine running guests of its own.
func cpuTotals(path string) (busy, total uint64, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: a fixed /proc path.
	if err != nil {
		return 0, 0, fmt.Errorf("reading CPU times: %w", err)
	}
	defer func() { _ = f.Close() }()

	line, _ := bufio.NewReader(f).ReadString('\n')
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("%w: %s has no aggregate cpu line", errProcFormat, path)
	}
	const idle, iowait, counted = 3, 4, 8 // positions within fields[1:], 0-based
	for i, field := range fields[1:min(len(fields), counted+1)] {
		v, perr := strconv.ParseUint(field, 10, 64)
		if perr != nil {
			return 0, 0, fmt.Errorf("%w: %s field %d: %w", errProcFormat, path, i+1, perr)
		}
		total += v
		if i != idle && i != iowait {
			busy += v
		}
	}
	return busy, total, nil
}

// meminfoField reads one "Name: N kB" line of /proc/meminfo, in bytes.
func meminfoField(path, name string) (uint64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a fixed /proc path.
	if err != nil {
		return 0, fmt.Errorf("reading memory info: %w", err)
	}
	defer func() { _ = f.Close() }()

	prefix := name + ":"
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		rest, ok := strings.CutPrefix(scanner.Text(), prefix)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		v, perr := strconv.ParseUint(fields[0], 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("%w: %s %s: %w", errProcFormat, path, name, perr)
		}
		// The kernel's "kB" is KiB.
		return v * 1024, nil
	}
	return 0, fmt.Errorf("%w: %s has no %s", errProcFormat, path, name)
}
