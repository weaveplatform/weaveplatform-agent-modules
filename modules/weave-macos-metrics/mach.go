//go:build darwin

package main

import (
	"fmt"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/libraries/libproc"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/libraries/machhost"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/libraries/machinit"
)

// Mach host_statistics64 flavors and their integer counts.
//
// The counts must be the full ones the kernel expects: host_statistics64
// validates the caller's count against the flavor and fails rather than
// returning a short answer, so asking for only the leading fields yields an
// error, or zeroes that look like an idle machine.
//
//   - HOST_CPU_LOAD_INFO_COUNT is CPU_STATE_MAX, 4 tick counters.
//   - HOST_VM_INFO64_COUNT is sizeof(vm_statistics64_data_t)/sizeof(integer_t),
//     38. Only free_count and inactive_count are read here.
const (
	hostCPULoadInfo   = 3
	cpuStateMax       = 4
	hostVMInfo64      = 4
	hostVMInfo64Count = 38
)

// Indices into host_cpu_load_info's cpu_ticks.
const (
	cpuStateUser = iota
	cpuStateSystem
	cpuStateIdle
	cpuStateNice
)

// Indices into vm_statistics64: free_count, active_count, inactive_count.
const (
	vmFreeCount     = 0
	vmInactiveCount = 2
)

// hostStatistics calls host_statistics64 on this host and returns the
// flavor's natural_t array, which is unsigned; the binding types it int32
// after the C prototype's integer_t.
func hostStatistics(flavor int32, count uint32) ([]uint32, error) {
	return readHostStatistics(machhost.Statistics64, flavor, count)
}

// statistics64 is host_statistics64's shape, so a test can answer short.
type statistics64 func(host uint32, flavor int32, out *int32, count *uint32) error

func readHostStatistics(call statistics64, flavor int32, count uint32) ([]uint32, error) {
	buf := make([]int32, count)
	n := count
	if err := call(machinit.HostSelf(), flavor, &buf[0], &n); err != nil {
		return nil, fmt.Errorf("host_statistics64 flavor %d: %w", flavor, err)
	}
	if n != count {
		return nil, fmt.Errorf(
			"%w: host_statistics64 flavor %d returned %d of %d values",
			errSysctlFormat,
			flavor,
			n,
			count,
		)
	}
	out := make([]uint32, count)
	for i, v := range buf {
		out[i] = uint32(v) //nolint:gosec // G115: natural_t is unsigned.
	}
	return out, nil
}

// processCount lists every pid through libproc. Called with no buffer,
// proc_listallpids answers an estimate with headroom, so the list is read
// into a buffer of that size and the count it fills is the answer.
func processCount() (int, error) { return countProcesses(libproc.Listallpids) }

func countProcesses(list func(buffer unsafe.Pointer, size int32) int32) (int, error) {
	estimate := list(nil, 0)
	if estimate <= 0 {
		return 0, fmt.Errorf(
			"%w: proc_listallpids estimated %d processes",
			errSysctlFormat,
			estimate,
		)
	}
	pids := make([]int32, estimate)
	n := list(unsafe.Pointer(&pids[0]), estimate*int32(unsafe.Sizeof(pids[0])))
	if n <= 0 {
		return 0, fmt.Errorf("%w: proc_listallpids listed %d processes", errSysctlFormat, n)
	}
	return int(n), nil
}
