//go:build windows

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/processstatus"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/system/systeminformation"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Against the real Win32 calls: a struct size or argument order that is wrong
// comes back as an error or zeroes, which look like an idle machine.
func TestMetricsReadTheRealSystem(t *testing.T) {
	m := newMetrics()
	var first weavewire.MetricsResponse
	if err := m.Sample(context.Background(), &first); err != nil {
		t.Fatal(err)
	}
	if first.CPUPercent != 0 {
		t.Errorf(
			"first sample reported %.1f%% CPU; it has nothing to compare against yet",
			first.CPUPercent,
		)
	}
	if first.MemoryTotalBytes == 0 || first.MemoryAvailableBytes == 0 ||
		first.MemoryAvailableBytes > first.MemoryTotalBytes {
		t.Errorf("memory available %d of %d", first.MemoryAvailableBytes, first.MemoryTotalBytes)
	}
	if first.SwapUsedBytes > first.SwapTotalBytes {
		t.Errorf("swap used %d of %d", first.SwapUsedBytes, first.SwapTotalBytes)
	}
	if first.LoadAverage != nil || first.ProcessCount == 0 {
		t.Errorf("load %v (want none), processes %d", first.LoadAverage, first.ProcessCount)
	}
	if len(first.Disks) != 1 || first.Disks[0].TotalBytes == 0 ||
		first.Disks[0].FreeBytes > first.Disks[0].TotalBytes {
		t.Errorf("disks = %+v", first.Disks)
	}

	spin()
	var second weavewire.MetricsResponse
	if err := m.Sample(context.Background(), &second); err != nil {
		t.Fatal(err)
	}
	if second.CPUPercent < 0 || second.CPUPercent > 100 {
		t.Errorf("CPU = %.2f%%, outside 0-100", second.CPUPercent)
	}
	t.Logf("%+v", second)
}

// Every source failing still yields a sample: a reading missing one figure is
// worth more to a host than no reading.
func TestMetricsAreBestEffort(t *testing.T) {
	errDown := errors.New("unavailable")
	m := &metrics{
		systemTimes:  func(_, _, _ *foundation.FILETIME) error { return errDown },
		memoryStatus: func(*systeminformation.MEMORYSTATUSEX) error { return errDown },
		performance:  func(*processstatus.PERFORMANCE_INFORMATION) error { return errDown },
		diskFree:     func(string, *uint64, *uint64, *uint64) error { return errDown },
		volume:       `C:\`,
	}
	var out weavewire.MetricsResponse
	if err := m.Sample(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if out.MemoryTotalBytes != 0 || out.ProcessCount != 0 || out.Disks != nil {
		t.Errorf("a failed source filled a field: %+v", out)
	}
}

func TestCPUFromSystemTimes(t *testing.T) {
	var kernel, user, idle uint64
	m := &metrics{
		systemTimes: func(i, k, u *foundation.FILETIME) error {
			*i, *k, *u = filetime(idle), filetime(kernel), filetime(user)
			return nil
		},
		memoryStatus: func(*systeminformation.MEMORYSTATUSEX) error { return errors.New("skip") },
		performance:  func(*processstatus.PERFORMANCE_INFORMATION) error { return errors.New("skip") },
		diskFree:     func(string, *uint64, *uint64, *uint64) error { return errors.New("skip") },
	}
	kernel, user, idle = 1<<33, 100, 1<<32
	var out weavewire.MetricsResponse
	_ = m.Sample(context.Background(), &out)
	// 400 more ticks of kernel time, 300 of them idle, and 100 of user time:
	// 200 busy out of 500.
	kernel, user, idle = 1<<33+400, 200, 1<<32+300
	_ = m.Sample(context.Background(), &out)
	if out.CPUPercent != 40 {
		t.Errorf("CPU = %v, want 40", out.CPUPercent)
	}
}

func filetime(v uint64) foundation.FILETIME {
	return foundation.FILETIME{DwLowDateTime: uint32(v), DwHighDateTime: uint32(v >> 32)}
}

func TestPageFile(t *testing.T) {
	const gib = 1 << 30
	for name, tc := range map[string]struct {
		mem         systeminformation.MEMORYSTATUSEX
		total, used uint64
	}{
		"no page file": {systeminformation.MEMORYSTATUSEX{UllTotalPhys: 8 * gib, UllAvailPhys: 4 * gib, UllTotalPageFile: 8 * gib, UllAvailPageFile: 4 * gib}, 0, 0},
		// 12 GiB commit limit over 8 GiB RAM: a 4 GiB page file. 7 GiB
		// committed against 5 GiB of RAM in use: 2 GiB in the page file.
		"in use":       {systeminformation.MEMORYSTATUSEX{UllTotalPhys: 8 * gib, UllAvailPhys: 3 * gib, UllTotalPageFile: 12 * gib, UllAvailPageFile: 5 * gib}, 4 * gib, 2 * gib},
		"unused":       {systeminformation.MEMORYSTATUSEX{UllTotalPhys: 8 * gib, UllAvailPhys: 3 * gib, UllTotalPageFile: 12 * gib, UllAvailPageFile: 10 * gib}, 4 * gib, 0},
		"inconsistent": {systeminformation.MEMORYSTATUSEX{UllTotalPhys: 8 * gib, UllTotalPageFile: 12 * gib, UllAvailPageFile: 13 * gib}, 0, 0},
	} {
		if total, used := pageFile(tc.mem); total != tc.total || used != tc.used {
			t.Errorf("%s: total %d used %d, want %d and %d", name, total, used, tc.total, tc.used)
		}
	}
}

func TestSystemVolume(t *testing.T) {
	if got := systemVolume("D:"); got != `D:\` {
		t.Errorf("systemVolume(D:) = %q", got)
	}
	if got := systemVolume(""); got != `C:\` {
		t.Errorf("systemVolume() = %q", got)
	}
}

// spin keeps one CPU busy long enough for the time counters to move.
func spin() {
	x := uint64(1)
	for i := range 50_000_000 {
		x = x*6364136223846793005 + uint64(i)
	}
	sink = x
}

var sink uint64
