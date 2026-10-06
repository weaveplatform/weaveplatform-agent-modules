package cliptransfer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// SpaceCheckBytes is how often a sink checks the disk again as it writes: an
// item may be refused at the start for want of room, but the disk is shared,
// and something else filling it mid-transfer must stop the transfer before
// the disk is full.
const SpaceCheckBytes = 32 << 20

// Sink is the receiving half of one item: it writes the stream to a partial
// file as it arrives, and only once the stream has ended, at the declared size
// and with the sender's digest, renames it to its final name. A partial file
// never takes the final name, so nothing half-written can be published.
type Sink struct {
	final, part string
	size        int64
	reserve     int64
	f           *os.File
	h           hash.Hash
	asm         weavewire.StreamAssembler
	written     int64
	checked     int64
	digest      string
}

// NewSink readies dir/name for an item of size bytes, refusing it with a
// no-space Failure when the disk lacks room for it and reserve.
func NewSink(dir, name string, size, reserve int64) (*Sink, error) {
	if _, err := CheckSpace(dir, size, reserve); err != nil {
		return nil, &Failure{Reason: weavewire.ClipboardReasonNoSpace, Err: err}
	}
	final := filepath.Join(dir, name)
	part := final + ".part"
	f, err := os.OpenFile(
		part,
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o600,
	) //nolint:gosec // G304: a staging path of ours
	if err != nil {
		return nil, fmt.Errorf("staging %s: %w", name, err)
	}
	return &Sink{final: final, part: part, size: size, reserve: reserve, f: f, h: sha256.New()}, nil
}

// Path is where the item is once it is whole.
func (s *Sink) Path() string { return s.final }

// Written is how many bytes have been written so far.
func (s *Sink) Written() int64 { return s.written }

// Digest is the hex SHA-256 of the item, once it is whole.
func (s *Sink) Digest() string { return s.digest }

// Accept writes one chunk and reports whether the item is now whole. Any
// error has ended the item: its partial file is gone, and the error is a
// Failure saying why (integrity, no-space, or the sender's own unreadable).
func (s *Sink) Accept(c weavewire.Chunk) (bool, error) {
	if s.f == nil {
		return false, &Failure{
			Reason: weavewire.ClipboardReasonIntegrity,
			Err:    fmt.Errorf("%w: a chunk after the end", ErrIntegrity),
		}
	}
	data, err := s.asm.Accept(c)
	if err != nil {
		return false, s.fail(weavewire.ClipboardReasonIntegrity, err)
	}
	if s.written+int64(len(data)) > s.size {
		return false, s.fail(weavewire.ClipboardReasonIntegrity,
			fmt.Errorf("%w: more than the %d bytes declared", ErrIntegrity, s.size))
	}
	if len(data) > 0 {
		if _, err := s.f.Write(data); err != nil {
			return false, s.fail(weavewire.ClipboardReasonNoSpace, err)
		}
		s.h.Write(data)
		s.written += int64(len(data))
	}
	if s.written-s.checked >= SpaceCheckBytes {
		s.checked = s.written
		if _, err := CheckSpace(filepath.Dir(s.part), s.size-s.written, s.reserve); err != nil {
			return false, s.fail(weavewire.ClipboardReasonNoSpace, err)
		}
	}
	if !s.asm.Done() {
		return false, nil
	}
	return true, s.finish(c.Digest)
}

// finish checks the ended stream and gives the item its final name.
func (s *Sink) finish(want string) error {
	if err := s.asm.Err(); err != nil {
		return s.fail(weavewire.ClipboardReasonUnreadable, err)
	}
	if s.written != s.size {
		return s.fail(weavewire.ClipboardReasonIntegrity,
			fmt.Errorf("%w: %d bytes arrived of the %d declared", ErrIntegrity, s.written, s.size))
	}
	got := hex.EncodeToString(s.h.Sum(nil))
	if want != got {
		return s.fail(weavewire.ClipboardReasonIntegrity,
			fmt.Errorf("%w: SHA-256 %s, the sender's %q", ErrIntegrity, got, want))
	}
	err := s.f.Close()
	s.f = nil
	if err == nil {
		err = os.Rename(s.part, s.final)
	}
	if err != nil {
		_ = os.Remove(s.part)
		return &Failure{Reason: weavewire.ClipboardReasonNoSpace, Err: err}
	}
	s.digest = got
	return nil
}

// fail ends the item for reason and removes what was written of it.
func (s *Sink) fail(reason string, err error) error {
	s.Abort()
	return &Failure{Reason: reason, Err: err}
}

// Abort ends the item where it stands and removes its partial file. It is
// safe to call more than once, and after the item is whole (it then does
// nothing).
func (s *Sink) Abort() {
	if s.f == nil {
		return
	}
	_ = s.f.Close()
	s.f = nil
	_ = os.Remove(s.part)
}
