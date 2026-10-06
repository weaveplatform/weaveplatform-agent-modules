package weaveclipboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/internal/cliptransfer"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// The guest's staging area: one directory per transfer under one root, each
// item in a numbered directory of its own, so two files of the same name from
// different host folders are both pasteable under that name.
//
// A transfer's directory is in flight while its items stream in, published
// once a set has put its files on the clipboard, and deleted when a newer
// transfer supersedes it or a newer set replaces it on the clipboard: the
// clipboard references no file of it any more.

// errSuperseded is an item a newer transfer, or a cancel, abandoned.
var errSuperseded = errors.New("superseded by a newer transfer")

// staging is the set transfer being received: the newest stage's.
type staging struct {
	id    string
	dir   string
	items map[string]*staged // by stream id
	n     int
}

// staged is one item of a set, streaming in or whole.
type staged struct {
	mu      sync.Mutex
	format  weavewire.ClipboardFormat
	sink    *cliptransfer.Sink
	acked   int64
	done    bool
	failure error // why it did not arrive whole; nil once whole
}

// stageRoot is the staging root, created on first use. The caller holds mu.
func (s *Service) stageRoot() (string, error) {
	if s.root != "" {
		return s.root, nil
	}
	dir := s.rootDir
	if dir == "" {
		d, err := os.MkdirTemp("", "weave-clipboard-")
		if err != nil {
			return "", fmt.Errorf("creating the staging directory: %w", err)
		}
		dir = d
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the staging directory: %w", err)
	}
	s.root = dir
	return dir, nil
}

// newTransferDir is a fresh directory for one transfer's items. The caller
// holds mu.
func (s *Service) newTransferDir() (string, error) {
	root, err := s.stageRoot()
	if err != nil {
		return "", err
	}
	s.dirs++
	dir := filepath.Join(root, "t"+strconv.Itoa(s.dirs))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating a staging directory: %w", err)
	}
	return dir, nil
}

// dropStaging abandons the set transfer being received: its items stop, and
// what they staged is deleted. The caller holds mu.
func (s *Service) dropStaging() {
	in := s.inbound
	if in == nil {
		return
	}
	s.inbound = nil
	for _, it := range in.items {
		it.mu.Lock()
		if !it.done {
			it.done = true
			it.failure = &cliptransfer.Failure{
				Reason: weavewire.ClipboardReasonCancelled, Err: errSuperseded,
			}
		}
		it.sink.Abort()
		it.mu.Unlock()
	}
	_ = os.RemoveAll(in.dir)
}

// handleStage readies one item of a set to stream in: it supersedes any
// older transfer, checks the disk has room for the item and the reserve, and
// opens the item's partial file.
func (s *Service) handleStage(_ context.Context, payload []byte) ([]byte, error) {
	var req weavewire.ClipboardStageRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("%w: decoding stage: %w", ErrBadRequest, err)
	}
	switch {
	case req.TransferID == "" || req.StreamID == "":
		return nil, fmt.Errorf("%w: a stage needs a transfer and a stream id", ErrBadRequest)
	case req.Format == "":
		return nil, fmt.Errorf("%w: a stage needs a format", ErrBadRequest)
	case req.Size < 0:
		return nil, fmt.Errorf("%w: %s has a negative size", ErrBadRequest, req.Format)
	}
	name := "data"
	if req.Format == weavewire.ClipboardFiles {
		n, err := fileName(req.Name)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
		}
		name = n
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// A stage can overtake the cancel the host sent after it (they are
	// handled side by side), or arrive as the module stops: either way the
	// transfer is over, and staging for it would leave files nobody claims.
	if s.stopped || req.TransferID == s.cancelled {
		return weavewire.EncodePayload(weavewire.ClipboardStageResponse{
			Refused: weavewire.ClipboardReasonCancelled,
		})
	}
	if s.inbound == nil || s.inbound.id != req.TransferID {
		s.dropStaging()
		dir, err := s.newTransferDir()
		if err != nil {
			return nil, err
		}
		s.inbound = &staging{id: req.TransferID, dir: dir, items: make(map[string]*staged)}
	}
	in := s.inbound
	if _, dup := in.items[req.StreamID]; dup {
		return nil, fmt.Errorf("%w: stream %q is already staged", ErrBadRequest, req.StreamID)
	}
	dir := filepath.Join(in.dir, strconv.Itoa(in.n))
	in.n++
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("staging %s: %w", name, err)
	}
	sink, err := cliptransfer.NewSink(dir, name, req.Size, s.reserve)
	if err != nil {
		_ = os.Remove(dir)
		if cliptransfer.ReasonOf(err, "") == weavewire.ClipboardReasonNoSpace {
			free, _ := cliptransfer.FreeBytes(in.dir)
			return weavewire.EncodePayload(weavewire.ClipboardStageResponse{
				Refused: weavewire.ClipboardReasonNoSpace, Free: free,
			})
		}
		return nil, err //nolint:wrapcheck // NewSink names the item
	}
	in.items[req.StreamID] = &staged{format: req.Format, sink: sink}
	return weavewire.EncodePayload(weavewire.ClipboardStageResponse{})
}

// handlePut writes one chunk of a staged item. It runs in the module's ordered
// queue, so an item's chunks are written in the order the host sent them; a
// chunk for an item no longer staged (cancelled, superseded, failed) is
// dropped. The host hears how the item is doing from KindClipboardStaged:
// every ClipboardCreditBytes written, which opens its window, and once at the
// end, whole or not.
func (s *Service) handlePut(ctx context.Context, payload []byte) ([]byte, error) {
	var c weavewire.Chunk
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("%w: decoding put: %w", ErrUpload, err)
	}
	s.mu.Lock()
	var it *staged
	if s.inbound != nil {
		it = s.inbound.items[c.StreamID]
	}
	s.mu.Unlock()
	if it == nil {
		return nil, nil
	}

	it.mu.Lock()
	if it.done {
		it.mu.Unlock()
		return nil, nil
	}
	whole, err := it.sink.Accept(c)
	written := it.sink.Written()
	ev := weavewire.ClipboardStaged{StreamID: c.StreamID, Acked: written}
	switch {
	case err != nil:
		it.done, it.failure = true, err
		ev.Done = true
		ev.Reason = cliptransfer.ReasonOf(err, weavewire.ClipboardReasonIntegrity)
		ev.Err = err.Error()
	case whole:
		it.done = true
		ev.Done = true
	case written-it.acked < weavewire.ClipboardCreditBytes:
		it.mu.Unlock()
		return nil, nil
	}
	it.acked = written
	it.mu.Unlock()
	if s.emit != nil {
		_ = s.emit.Emit(ctx, weavewire.KindClipboardStaged, ev)
	}
	return nil, nil
}

// handleCancel abandons a transfer at once: a set's staging and a get's
// streams alike.
func (s *Service) handleCancel(_ context.Context, payload []byte) ([]byte, error) {
	var req weavewire.ClipboardCancel
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("%w: decoding cancel: %w", ErrBadRequest, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = req.TransferID
	if s.inbound != nil && s.inbound.id == req.TransferID {
		s.dropStaging()
	}
	if s.offer != nil && s.offer.id == req.TransferID {
		s.dropOffer()
	}
	return nil, nil
}

// claimStaged takes the staged items a set names out of the transfer being
// received: each must be whole. It returns the transfer's directory, which the
// set publishes. The caller holds mu.
func (s *Service) claimStaged(items []weavewire.ClipboardItem) (string, map[string]string, error) {
	paths := make(map[string]string)
	in := s.inbound
	for _, it := range items {
		if it.Stream == "" {
			continue
		}
		var st *staged
		if in != nil {
			st = in.items[it.Stream]
		}
		if st == nil {
			return "", nil, fmt.Errorf("%w: nothing is staged as %q", ErrUpload, it.Stream)
		}
		st.mu.Lock()
		done, failure, path, size := st.done, st.failure, st.sink.Path(), st.sink.Written()
		st.mu.Unlock()
		switch {
		case !done:
			return "", nil, fmt.Errorf("%w: %q is still streaming", ErrUpload, it.Stream)
		case failure != nil:
			return "", nil, fmt.Errorf("%w: %q: %w", ErrUpload, it.Stream, failure)
		case size != it.Size:
			return "", nil, fmt.Errorf("%w: %q staged %d bytes and the set declares %d",
				ErrUpload, it.Stream, size, it.Size)
		}
		paths[it.Stream] = path
	}
	if len(paths) == 0 {
		return "", paths, nil // nothing staged: an inline set's files get a directory of their own
	}
	return in.dir, paths, nil
}
