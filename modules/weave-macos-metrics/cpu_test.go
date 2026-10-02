//go:build darwin

package main

import "testing"

func TestCPUMeterMeasuresTheInterval(t *testing.T) {
	var m cpuMeter
	if got := m.percent(500, 1000); got != 0 {
		t.Errorf("first reading = %v, want 0: there is no interval yet", got)
	}
	if got := m.percent(750, 1500); got != 50 {
		t.Errorf("second reading = %v, want 50", got)
	}
	if got := m.percent(750, 1500); got != 0 {
		t.Errorf("no time passed = %v, want 0", got)
	}
	if got := m.percent(10, 20); got != 0 {
		t.Errorf("counters went backwards = %v, want 0", got)
	}
	if got := m.percent(20, 40); got != 50 {
		t.Errorf("after a reset = %v, want 50 against the new baseline", got)
	}
}
