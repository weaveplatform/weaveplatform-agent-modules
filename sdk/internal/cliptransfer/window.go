package cliptransfer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

var (
	// ErrStalled reports a stream whose receiver acknowledged nothing for the
	// idle timeout: it went away mid-transfer.
	ErrStalled = errors.New("the receiver stopped acknowledging")
	// ErrEnded reports a send on a stream its receiver has already ended.
	ErrEnded = errors.New("the receiver ended the stream")
)

// Window is a sender's view of one stream's flow control: how much the
// receiver has acknowledged, and whether it has ended the stream — taken it
// whole, given up on it, or been cancelled.
type Window struct {
	mu    sync.Mutex
	acked int64
	ended bool
	err   error
	wake  chan struct{} // closed and replaced on every change
}

// NewWindow is a window with nothing acknowledged.
func NewWindow() *Window { return &Window{wake: make(chan struct{})} }

// Ack records that the receiver has written n bytes in all. Acknowledgements
// are cumulative, so one that arrives late is ignored.
func (w *Window) Ack(n int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n <= w.acked || w.ended {
		return
	}
	w.acked = n
	w.signal()
}

// Acked is how many bytes the receiver has acknowledged.
func (w *Window) Acked() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.acked
}

// End ends the stream with err: nil for an item the receiver took whole,
// otherwise why it did not. The first end is kept.
func (w *Window) End(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return
	}
	w.ended, w.err = true, err
	w.signal()
}

// signal wakes every waiter. The caller holds mu.
func (w *Window) signal() {
	close(w.wake)
	w.wake = make(chan struct{})
}

// Wait blocks until the sender, having sent sent bytes, may send more: while
// sent is ClipboardWindowBytes or more ahead of what is acknowledged, it waits
// for an acknowledgement. It fails when the stream has ended (ErrEnded, or the
// receiver's reason), ctx ends, or nothing is acknowledged for idle (zero
// waits for ever).
func (w *Window) Wait(ctx context.Context, sent int64, idle time.Duration) error {
	return w.wait(ctx, idle, func() (bool, error) {
		if w.ended {
			if w.err != nil {
				return true, w.err
			}
			return true, ErrEnded
		}
		return sent-w.acked < weavewire.ClipboardWindowBytes, nil
	})
}

// WaitEnd blocks until the receiver ends the stream and returns its verdict:
// nil when it took the item whole. It fails as Wait does when ctx ends or
// nothing moves for idle.
func (w *Window) WaitEnd(ctx context.Context, idle time.Duration) error {
	return w.wait(ctx, idle, func() (bool, error) { return w.ended, w.err })
}

// wait re-evaluates ready on every change until it reports true. The idle
// timer restarts on every change, so a slow stream that keeps moving is never
// mistaken for a dead one.
func (w *Window) wait(ctx context.Context, idle time.Duration, ready func() (bool, error)) error {
	for {
		w.mu.Lock()
		ok, err := ready()
		wake := w.wake
		w.mu.Unlock()
		if ok {
			return err
		}
		var stall <-chan time.Time
		var t *time.Timer
		if idle > 0 {
			t = time.NewTimer(idle)
			stall = t.C
		}
		select {
		case <-wake:
		case <-stall:
			return ErrStalled
		case <-ctx.Done():
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}
