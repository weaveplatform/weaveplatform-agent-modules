//go:build linux

package main

import "sync"

// cpuMeter turns the kernel's cumulative busy and total CPU counters into
// utilisation over the interval since the previous reading.
//
// It needs two readings to mean anything, so the first reports 0 rather than
// utilisation since boot, which would look plausible and be wrong. A counter
// that went backwards (a reset, or a reading from a different source) also
// reports 0, and becomes the new baseline.
type cpuMeter struct {
	mu     sync.Mutex
	busy   uint64
	total  uint64
	primed bool
}

func (m *cpuMeter) percent(busy, total uint64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	prevBusy, prevTotal, primed := m.busy, m.total, m.primed
	m.busy, m.total, m.primed = busy, total, true
	if !primed || total <= prevTotal || busy < prevBusy {
		return 0
	}
	return float64(busy-prevBusy) / float64(total-prevTotal) * 100
}
