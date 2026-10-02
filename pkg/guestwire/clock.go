package guestwire

import "time"

// A suspended or snapshotted VM resumes with the clock it stopped with, and
// nothing inside notices: certificates fail validation, scheduled work fires
// late or all at once, and the guest's logs stop lining up with the host's.
// Only the host knows how long the guest was away, so only the host can
// correct it.

const (
	KindTimeGet = "guestweave.time.get"
	KindTimeSet = "guestweave.time.set"

	KindMetricsSample = "guestweave.metrics.sample"
)

// TimeResponse reports the guest's clock.
//
// UptimeSeconds is what separates "running for three days and drifted" from
// "resumed from a snapshot and is three days behind". Zone is for display; the
// clock itself is always UTC on the wire.
type TimeResponse struct {
	UnixNano      int64  `json:"unix_nano"`
	UptimeSeconds uint64 `json:"uptime_seconds,omitempty"`
	Zone          string `json:"zone,omitempty"`
	OffsetSeconds int    `json:"offset_seconds,omitempty"`
}

// TimeSetRequest asks the guest to set its clock to UnixNano.
//
// An absolute time rather than an offset to apply: an offset computed by the
// host would be stale by however long the message took to arrive, and the two
// ends share no clock to measure that with.
//
// MaxSkewSeconds guards the other direction — a host with its own broken clock
// dragging a healthy guest along with it. Zero means no limit.
type TimeSetRequest struct {
	UnixNano       int64 `json:"unix_nano"`
	MaxSkewSeconds int64 `json:"max_skew_seconds,omitempty"`
}

// TimeSetResponse reports what the correction did. PreviousUnixNano is the only
// record of how far the guest had drifted; SkewNanos is positive when it was
// behind.
type TimeSetResponse struct {
	PreviousUnixNano int64 `json:"previous_unix_nano"`
	SkewNanos        int64 `json:"skew_nanos"`
}

func (r TimeSetResponse) Skew() time.Duration { return time.Duration(r.SkewNanos) }

// MetricsResponse is one live resource sample.
//
// A sample rather than a stream: the host picks the cadence, and a guest
// pushing on its own schedule would compete with exec output for a channel they
// share.
type MetricsResponse struct {
	SampledAt time.Time `json:"sampled_at"`

	// CPUPercent is utilisation over the interval since the previous sample,
	// so the FIRST sample after the agent starts reports 0 rather than
	// utilisation-since-boot, which would look plausible and be wrong.
	CPUPercent float64 `json:"cpu_percent"`
	// LoadAverage is 1/5/15 minutes where the OS has it; Windows does not.
	LoadAverage []float64 `json:"load_average,omitempty"`

	MemoryTotalBytes     uint64 `json:"memory_total_bytes,omitempty"`
	MemoryAvailableBytes uint64 `json:"memory_available_bytes,omitempty"`
	SwapTotalBytes       uint64 `json:"swap_total_bytes,omitempty"`
	SwapUsedBytes        uint64 `json:"swap_used_bytes,omitempty"`

	Disks        []DiskUsage `json:"disks,omitempty"`
	ProcessCount int         `json:"process_count,omitempty"`
}

type DiskUsage struct {
	Mountpoint string `json:"mountpoint"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}
