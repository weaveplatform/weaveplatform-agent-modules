//go:build darwin

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"syscall"
	"testing"
	"unsafe"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Against the real Mach, Foundation, libproc and sysctl interfaces.
// host_statistics64 through purego is the part most worth exercising: a wrong
// flavor or count comes back as an error or zeroes, which look like a quiet
// machine rather than a broken call.
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
	// Not asserted above zero: macOS rate-limits host_statistics64 for an
	// unentitled caller and answers repeated calls from a cache, so two
	// samples this close together can see the same ticks.
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
		hostStats:      func(int32, uint32) ([]uint32, error) { return nil, errDown },
		physicalMemory: func() uint64 { return 0 },
		sysctl:         func(string) (string, error) { return "", errDown },
		statfs:         func(string, *syscall.Statfs_t) error { return errDown },
		processCount:   func() (int, error) { return 0, errDown },
		root:           "/",
	}
	var out weavewire.MetricsResponse
	if err := m.Sample(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if out.MemoryAvailableBytes != 0 || out.LoadAverage != nil || out.Disks != nil ||
		out.ProcessCount != 0 {
		t.Errorf("a failed source filled a field: %+v", out)
	}

	// A sysctl that answers with the wrong layout fills nothing either.
	m.sysctl = func(string) (string, error) { return "short", nil }
	out = weavewire.MetricsResponse{}
	if err := m.Sample(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if out.LoadAverage != nil || out.SwapTotalBytes != 0 {
		t.Errorf("a malformed sysctl filled a field: %+v", out)
	}
}

func loadavgBytes(l0, l1, l2 uint32, scale uint64) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b[0:], l0)
	binary.LittleEndian.PutUint32(b[4:], l1)
	binary.LittleEndian.PutUint32(b[8:], l2)
	binary.LittleEndian.PutUint64(b[16:], scale)
	return b
}

func TestParseLoadavg(t *testing.T) {
	full := loadavgBytes(2048, 1024, 512, 2048)
	for name, raw := range map[string][]byte{
		"whole":                   full,
		"trailing NUL dropped":    full[:23],
		"as syscall.Sysctl gives": []byte(mustSysctlTrim(full)),
	} {
		got, err := parseLoadavg(string(raw))
		if err != nil || len(got) != 3 || got[0] != 1 || got[1] != 0.5 || got[2] != 0.25 {
			t.Errorf("%s: %v, %v", name, got, err)
		}
	}
	for name, raw := range map[string][]byte{
		"too short": full[:20],
		"too long":  append(full, 0),
		"no scale":  loadavgBytes(1, 1, 1, 0),
	} {
		if _, err := parseLoadavg(string(raw)); !errors.Is(err, errSysctlFormat) {
			t.Errorf("%s: err = %v, want errSysctlFormat", name, err)
		}
	}
}

// mustSysctlTrim mimics syscall.Sysctl, which drops one trailing NUL.
func mustSysctlTrim(b []byte) string {
	if n := len(b); n > 0 && b[n-1] == 0 {
		return string(b[:n-1])
	}
	return string(b)
}

func TestParseSwapusage(t *testing.T) {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint64(b[0:], 4<<30)
	binary.LittleEndian.PutUint64(b[8:], 3<<30)
	binary.LittleEndian.PutUint64(b[16:], 1<<30)
	binary.LittleEndian.PutUint32(b[24:], 16384)
	binary.LittleEndian.PutUint32(b[28:], 1)
	total, used, err := parseSwapusage(mustSysctlTrim(b))
	if err != nil || total != 4<<30 || used != 1<<30 {
		t.Errorf("swap = %d used of %d, %v", used, total, err)
	}
	if _, _, err := parseSwapusage("short"); !errors.Is(err, errSysctlFormat) {
		t.Errorf("err = %v, want errSysctlFormat", err)
	}
}

// A flavor the kernel does not know, or a count that does not match it, is an
// error rather than a buffer of zeroes.
func TestHostStatisticsRefusesAWrongCount(t *testing.T) {
	if _, err := hostStatistics(hostCPULoadInfo, cpuStateMax); err != nil {
		t.Fatalf("real flavor: %v", err)
	}
	if _, err := hostStatistics(hostVMInfo64, 2); err == nil {
		t.Error("a short count for HOST_VM_INFO64 was accepted")
	}
	if _, err := hostStatistics(9999, 4); err == nil {
		t.Error("an unknown flavor was accepted")
	}
}

func TestHostStatisticsRefusesAShortAnswer(t *testing.T) {
	short := func(_ uint32, _ int32, _ *int32, n *uint32) error { *n--; return nil }
	if _, err := readHostStatistics(
		short,
		hostCPULoadInfo,
		cpuStateMax,
	); !errors.Is(
		err,
		errSysctlFormat,
	) {
		t.Errorf("err = %v, want errSysctlFormat", err)
	}
}

func TestProcessCount(t *testing.T) {
	n, err := processCount()
	if err != nil || n < 2 {
		t.Fatalf("processCount() = %d, %v", n, err)
	}
	failing := func(answers ...int32) func(unsafe.Pointer, int32) int32 {
		return func(unsafe.Pointer, int32) int32 { v := answers[0]; answers = answers[1:]; return v }
	}
	if _, err := countProcesses(failing(-1)); !errors.Is(err, errSysctlFormat) {
		t.Errorf("failed estimate: err = %v", err)
	}
	if _, err := countProcesses(failing(10, -1)); !errors.Is(err, errSysctlFormat) {
		t.Errorf("failed listing: err = %v", err)
	}
}

// spin keeps one CPU busy long enough for the tick counters to move.
func spin() {
	x := uint64(1)
	for i := range 50_000_000 {
		x = x*6364136223846793005 + uint64(i)
	}
	sink = x
}

var sink uint64
