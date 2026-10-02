package weaveexec

import (
	"io"
	"sync/atomic"
	"time"
)

// A terminal's output can outlive the process that wrote it: on macOS the last
// close of a terminal's child side discards unread output, and Windows' console
// host renders asynchronously. Both terminal implementations therefore keep
// the terminal open after the child exits until its output has gone quiet.

// Settling bounds, vars so tests can shorten them.
var (
	settleQuiet = 200 * time.Millisecond
	settleMax   = 2 * time.Second
)

// settle returns once output has been quiet for settleQuiet after the child
// exited, or after settleMax, so a reader that has stopped reading cannot stall
// the end of the stream.
func settle(last *atomic.Int64) {
	deadline := time.Now().Add(settleMax)
	for time.Now().Before(deadline) {
		if time.Since(time.Unix(0, last.Load())) >= settleQuiet {
			return
		}
		time.Sleep(settleQuiet / 8)
	}
}

// stampedReader records when output was last read, for settle.
type stampedReader struct {
	r    io.Reader
	last *atomic.Int64
}

func (s stampedReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if n > 0 {
		s.last.Store(time.Now().UnixNano())
	}
	return n, err //nolint:wrapcheck // io.Reader contract: io.EOF must pass through unwrapped
}
