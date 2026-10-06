package cliptransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Sender streams one item as chunks of one stream, under the stream's window.
type Sender struct {
	// Size is exactly how many bytes the item has: the stream carries that
	// many, and a source that has more or fewer is a failure.
	Size int64
	// Window is the stream's flow control, acknowledged by the receiver.
	Window *Window
	// Idle is how long the receiver may acknowledge nothing before the
	// stream is abandoned as stalled; zero waits for ever.
	Idle time.Duration
	// Pace, when set, is waited on before each chunk with its size: the
	// bandwidth policy.
	Pace func(ctx context.Context, n int) error
	// Emit sends one chunk; the stream id is the caller's to fill in.
	Emit func(ctx context.Context, c weavewire.Chunk) error
	// Progress, when set, is told how many bytes have been sent in all after
	// each chunk.
	Progress func(sent int64)
}

// Stream sends exactly s.Size bytes of r and ends with an EOF carrying their
// SHA-256, which it returns.
//
// A read that fails, or a source with more or fewer bytes than Size (a file
// changed since it was sized), ends the stream with an EOF carrying the
// failure and returns an unreadable Failure. When the window ends, stalls or
// ctx ends, the stream is ended the same way, best effort, so the receiver
// drops what it staged rather than waiting for the rest.
func (s Sender) Stream(ctx context.Context, r io.Reader) (string, error) {
	var (
		seq  uint64
		sent int64
		h    = sha256.New()
		buf  = make([]byte, weavewire.MaxChunkBytes)
	)
	emit := func(c weavewire.Chunk) error {
		c.Seq = seq
		seq++
		return s.Emit(ctx, c)
	}
	abort := func(err error) error {
		// Sent even when ctx has ended, so the receiver drops what it
		// staged at once rather than waiting out its idle timeout.
		c := weavewire.Chunk{EOF: true, Err: err.Error(), Seq: seq}
		seq++
		_ = s.Emit(context.WithoutCancel(ctx), c)
		return err
	}
	for sent < s.Size {
		if err := s.Window.Wait(ctx, sent, s.Idle); err != nil {
			return "", abort(err)
		}
		want := min(int64(len(buf)), s.Size-sent)
		n, err := io.ReadFull(r, buf[:want])
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				err = fmt.Errorf(
					"%w: it ended %d bytes into %d",
					ErrSourceChanged,
					sent+int64(n),
					s.Size,
				)
			}
			return "", abort(&Failure{Reason: weavewire.ClipboardReasonUnreadable, Err: err})
		}
		if s.Pace != nil {
			if err := s.Pace(ctx, n); err != nil {
				return "", abort(err)
			}
		}
		h.Write(buf[:n])
		if err := emit(weavewire.Chunk{Data: buf[:n]}); err != nil {
			return "", err
		}
		sent += int64(n)
		if s.Progress != nil {
			s.Progress(sent)
		}
	}
	// A source that grew since it was sized is not the item that was
	// offered: its first Size bytes would arrive intact and wrong.
	if n, _ := r.Read(buf[:1]); n > 0 {
		return "", abort(&Failure{
			Reason: weavewire.ClipboardReasonUnreadable,
			Err:    fmt.Errorf("%w: it has more than %d bytes", ErrSourceChanged, s.Size),
		})
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if err := emit(weavewire.Chunk{EOF: true, Digest: digest}); err != nil {
		return "", err
	}
	return digest, nil
}
