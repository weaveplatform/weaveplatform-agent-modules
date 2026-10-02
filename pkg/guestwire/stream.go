package guestwire

import (
	"errors"
	"fmt"
)

// Stream failures, for callers that branch on why a stream is unusable.
var (
	// ErrStreamGap: a chunk was lost, so nothing after it can be trusted.
	ErrStreamGap = errors.New("guestwire: stream lost chunks")
	// ErrAfterEOF: a chunk arrived for a stream that had already ended.
	ErrAfterEOF = errors.New("guestwire: chunk after EOF")
	// ErrChunkTooLarge: a chunk exceeded MaxChunkBytes.
	ErrChunkTooLarge = errors.New("guestwire: chunk over the size cap")
	// ErrStreamFailed: the producer ended the stream with an error.
	ErrStreamFailed = errors.New("guestwire: stream ended early")
)

// StreamAssembler is the receiving half of a Chunk stream. Both ends use it —
// the host reassembling exec stdout or a file it pulled, the guest reassembling
// exec stdin or a file being pushed — so the rules that decide whether a
// transfer was intact exist exactly once.
//
// The rules it enforces:
//
//   - Sequence numbers must be contiguous from 0. A gap means chunks were lost
//     and everything after them is suspect, so it is an error, not a warning.
//     Silent truncation is the failure mode this type exists to prevent.
//   - A stream terminates only on an EOF chunk. A stream that just stops is a
//     dead channel; the caller learns that from the channel, and Done() staying
//     false is what stops it from being mistaken for success.
//   - Nothing is accepted after EOF.
//
// It is not safe for concurrent use; a stream has one consumer.
type StreamAssembler struct {
	nextSeq uint64
	done    bool
	err     string
}

// Accept takes the next chunk of a stream and returns its payload bytes.
//
// The returned slice aliases c.Data — copy it if you retain it past the next
// Accept. Callers that write straight into a file or an io.Writer do not need
// to.
func (a *StreamAssembler) Accept(c Chunk) ([]byte, error) {
	if a.done {
		return nil, fmt.Errorf("%w: chunk %d for stream %q", ErrAfterEOF, c.Seq, c.StreamID)
	}
	if c.Seq != a.nextSeq {
		return nil, fmt.Errorf("%w: stream %q expected seq %d, got %d",
			ErrStreamGap, c.StreamID, a.nextSeq, c.Seq)
	}
	if len(c.Data) > MaxChunkBytes {
		return nil, fmt.Errorf("%w: stream %q chunk %d is %d bytes, cap %d",
			ErrChunkTooLarge, c.StreamID, c.Seq, len(c.Data), MaxChunkBytes)
	}
	a.nextSeq++
	if c.EOF {
		a.done = true
		a.err = c.Err
	}
	return c.Data, nil
}

// Done reports whether the terminating EOF chunk has been seen. A consumer that
// stops reading while this is false has a truncated stream, however clean the
// bytes it did receive looked.
func (a *StreamAssembler) Done() bool { return a.done }

// Err reports the producer's failure, if the stream ended carrying one. It is
// meaningful only once Done reports true.
func (a *StreamAssembler) Err() error {
	if a.err == "" {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrStreamFailed, a.err)
}
