//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Against the real kernel: the counters are only worth anything if they read
// back plausible values from the machine running the test.
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
	if len(first.LoadAverage) != 3 || first.ProcessCount == 0 {
		t.Errorf("load %v, processes %d", first.LoadAverage, first.ProcessCount)
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
	if second.CPUPercent <= 0 || second.CPUPercent > 100 {
		t.Errorf("CPU = %.2f%% after spinning, want within (0, 100]", second.CPUPercent)
	}
	t.Logf("%+v", second)
}

// Every source failing still yields a sample: a reading missing one figure is
// worth more to a host than no reading.
func TestMetricsAreBestEffort(t *testing.T) {
	errDown := errors.New("unavailable")
	m := &metrics{
		proc:    t.TempDir(), // no stat, no meminfo
		root:    "/",
		sysinfo: func(*unix.Sysinfo_t) error { return errDown },
		statfs:  func(string, *unix.Statfs_t) error { return errDown },
	}
	var out weavewire.MetricsResponse
	if err := m.Sample(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if out.MemoryTotalBytes != 0 || out.MemoryAvailableBytes != 0 || out.LoadAverage != nil ||
		out.Disks != nil {
		t.Errorf("a failed source filled a field: %+v", out)
	}
}

func writeProc(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCPUTotals(t *testing.T) {
	// user nice system idle iowait irq softirq steal guest guest_nice
	busy, total, err := cpuTotals(
		writeProc(t, "stat", "cpu  10 20 30 400 50 6 7 8 1000 2000\ncpu0 1 2 3 4 5\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	// guest and guest_nice are inside user and nice already, so not counted.
	if busy != 10+20+30+6+7+8 || total != busy+400+50 {
		t.Errorf("busy %d total %d", busy, total)
	}

	// A kernel from before steal still reads.
	if busy, total, err := cpuTotals(
		writeProc(t, "stat", "cpu 1 2 3 4 5\n"),
	); err != nil || busy != 6 ||
		total != 15 {
		t.Errorf("short line: busy %d total %d err %v", busy, total, err)
	}

	for name, content := range map[string]string{
		"no aggregate line": "cpu0 1 2 3 4 5\n",
		"too short":         "cpu 1 2 3\n",
		"empty":             "",
		"not a number":      "cpu 1 x 3 4 5\n",
	} {
		if _, _, err := cpuTotals(writeProc(t, "stat", content)); !errors.Is(err, errProcFormat) {
			t.Errorf("%s: err = %v, want errProcFormat", name, err)
		}
	}
}

func TestMeminfoField(t *testing.T) {
	path := writeProc(
		t,
		"meminfo",
		"MemTotal:       1000 kB\nMemAvailable:    250 kB\nEmpty:\nBad: x kB\n",
	)
	if v, err := meminfoField(path, "MemAvailable"); err != nil || v != 250*1024 {
		t.Errorf("MemAvailable = %d, %v", v, err)
	}
	for _, name := range []string{"Missing", "Empty", "Bad"} {
		if _, err := meminfoField(path, name); !errors.Is(err, errProcFormat) {
			t.Errorf("%s: err = %v, want errProcFormat", name, err)
		}
	}
	if _, err := meminfoField(filepath.Join(t.TempDir(), "absent"), "MemAvailable"); err == nil {
		t.Error("a missing meminfo read as a value")
	}
}

// spin keeps one CPU busy long enough for the jiffy counters to move.
func spin() {
	x := uint64(1)
	for i := range 50_000_000 {
		x = x*6364136223846793005 + uint64(i)
	}
	sink = x
}

var sink uint64
