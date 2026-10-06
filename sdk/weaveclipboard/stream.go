package weaveclipboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/internal/cliptransfer"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// sendIdle is how long a stream to the host may go unacknowledged before it is
// abandoned: the host went away, or its engine stopped mid-transfer. A host
// pacing a transfer to its bandwidth policy acknowledges each chunk at the
// policy's rate, so this is far longer than even a slow policy takes over one;
// a stream abandoned late costs only an open file.
var sendIdle = 10 * time.Minute

// offer is the newest streaming get: the files it listed as Deferred, for the
// host to fetch, and the context its streams run under, which a newer get or a
// cancel ends.
type offer struct {
	id     string
	files  []File
	ctx    context.Context
	cancel context.CancelFunc
}

// newOffer replaces the current offer with one for a get. Whatever the older
// one still streams stops. The caller holds mu.
func (s *Service) newOffer(id string, files []File) *offer {
	s.dropOffer()
	ctx, cancel := context.WithCancel(context.Background())
	s.offer = &offer{id: id, files: files, ctx: ctx, cancel: cancel}
	return s.offer
}

// dropOffer ends the current offer's streams. The caller holds mu.
func (s *Service) dropOffer() {
	if s.offer != nil {
		s.offer.cancel()
		s.offer = nil
	}
}

// openWindow registers a stream to the host, so the credits for it find it. The
// window is registered before the reply that announces the stream goes out:
// the host may credit it before this side has sent anything.
func (s *Service) openWindow(id string) *cliptransfer.Window {
	w := cliptransfer.NewWindow()
	s.mu.Lock()
	s.windows[id] = w
	s.mu.Unlock()
	return w
}

func (s *Service) closeWindow(id string) {
	s.mu.Lock()
	delete(s.windows, id)
	s.mu.Unlock()
}

// handleCredit opens a stream's window as far as the host has written it.
func (s *Service) handleCredit(_ context.Context, payload []byte) ([]byte, error) {
	var c weavewire.ClipboardCredit
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("%w: decoding credit: %w", ErrBadRequest, err)
	}
	s.mu.Lock()
	w := s.windows[c.StreamID]
	s.mu.Unlock()
	if w != nil {
		w.Ack(c.Acked)
	}
	return nil, nil
}

// stream sends size bytes of r as download chunks under id, as fast as the
// host's credits allow, ending with an EOF that carries their digest — or the
// failure, when the stream could not finish. It stops when ctx ends.
func (s *Service) stream(
	ctx context.Context,
	id string,
	w *cliptransfer.Window,
	size int64,
	r io.Reader,
) error {
	defer s.closeWindow(id)
	_, err := cliptransfer.Sender{
		Size:   size,
		Window: w,
		Idle:   sendIdle,
		Emit: func(ctx context.Context, c weavewire.Chunk) error {
			c.StreamID = id
			return s.emit.Emit(ctx, weavewire.KindClipboardDownload, c)
		},
	}.Stream(ctx, r)
	return err //nolint:wrapcheck // the stream's own failure, reported on its EOF
}

// streamingGet answers a streaming get: files listed as Deferred, the rest
// inline or as a flow-controlled download stream, with no ceiling in total.
// Each item is judged on its own against the host's per-item cap; one over it
// is listed as omitted.
func (s *Service) streamingGet(
	ctx context.Context,
	req weavewire.ClipboardGetRequest,
) ([]byte, func(), error) {
	if req.TransferID == "" {
		return nil, nil, fmt.Errorf("%w: a streaming get needs a transfer id", ErrBadRequest)
	}
	limit := req.MaxBytes
	if limit <= 0 || limit > weavewire.MaxClipboardRepresentationBytes {
		limit = weavewire.MaxClipboardRepresentationBytes
	}
	c, err := s.b.Read(ctx, req.Formats, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("weaveclipboard: reading: %w", err)
	}
	resp := weavewire.ClipboardGetResponse{ChangeToken: c.ChangeToken}
	var files []File
	if len(req.Formats) == 0 || containsFormat(req.Formats, weavewire.ClipboardFiles) {
		for _, f := range c.Files {
			it := weavewire.ClipboardItem{
				Format: weavewire.ClipboardFiles,
				Name:   f.Name,
				Size:   f.Size,
			}
			if req.MaxBytes > 0 && f.Size > req.MaxBytes {
				resp.Omitted = append(resp.Omitted, it)
				continue
			}
			it.Deferred = true
			files = append(files, f)
			resp.Items = append(resp.Items, it)
		}
	}
	var (
		data  [][]byte
		total int64
	)
	for _, it := range c.Items {
		if len(req.Formats) > 0 && !containsFormat(req.Formats, it.Format) {
			continue
		}
		if it.Data != nil {
			it.Size = int64(len(it.Data))
		}
		if it.Size > limit || (it.Data == nil && it.Size > 0) {
			resp.Omitted = append(resp.Omitted, weavewire.ClipboardItem{
				Format: it.Format, Name: it.Name, Size: it.Size,
			})
			continue
		}
		total += it.Size
		data = append(data, it.Data)
		resp.Items = append(resp.Items, it)
	}

	s.mu.Lock()
	o := s.newOffer(req.TransferID, files)
	s.mu.Unlock()

	if total <= weavewire.ClipboardInlineBytes {
		out, err := weavewire.EncodePayload(resp)
		return out, nil, err
	}
	resp.Streamed = true
	for i := range resp.Items {
		resp.Items[i].Data = nil
	}
	out, err := weavewire.EncodePayload(resp)
	if err != nil {
		return nil, nil, err
	}
	w := s.openWindow(req.TransferID)
	after := func() {
		r := io.MultiReader(readers(data)...)
		_ = s.stream(o.ctx, req.TransferID, w, total, r)
	}
	return out, after, nil
}

func readers(data [][]byte) []io.Reader {
	out := make([]io.Reader, len(data))
	for i, d := range data {
		out[i] = bytes.NewReader(d)
	}
	return out
}

// errNotOffered refuses a fetch of a file the newest get did not offer.
var errNotOffered = errors.New("weaveclipboard: not offered")

// handleFetch streams one Deferred file of the newest streaming get from disk.
// The reply carries the file's size; its bytes follow as download chunks under
// the fetch's StreamID, flow-controlled by the host's credits. A file that is
// gone, or has changed size, since the get listed it is refused: it is not the
// file that was copied.
func (s *Service) handleFetch(_ context.Context, payload []byte) ([]byte, func(), error) {
	var req weavewire.ClipboardFetchRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, nil, fmt.Errorf("%w: decoding fetch: %w", ErrBadRequest, err)
	}
	if req.StreamID == "" {
		return nil, nil, fmt.Errorf("%w: a fetch needs a stream id", ErrBadRequest)
	}
	s.mu.Lock()
	o := s.offer
	s.mu.Unlock()
	if o == nil || o.id != req.TransferID || req.Index < 0 || req.Index >= len(o.files) {
		return nil, nil, fmt.Errorf(
			"%w: file %d of %q (a newer copy superseded it, or it never was)",
			errNotOffered,
			req.Index,
			req.TransferID,
		)
	}
	f := o.files[req.Index]
	file, err := os.Open(f.Path) //nolint:gosec // G304: a path the user copied
	if err != nil {
		return nil, nil, fmt.Errorf(
			"%s: %w: %w",
			weavewire.ClipboardReasonUnreadable,
			errNotOffered,
			err,
		)
	}
	if fi, err := file.Stat(); err != nil || fi.Size() != f.Size {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%s: %w: %s changed since it was copied",
			weavewire.ClipboardReasonUnreadable, errNotOffered, f.Name)
	}
	out, err := weavewire.EncodePayload(weavewire.ClipboardFetchResponse{Size: f.Size})
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	w := s.openWindow(req.StreamID)
	after := func() {
		defer file.Close() //nolint:errcheck // read only
		_ = s.stream(o.ctx, req.StreamID, w, f.Size, file)
	}
	return out, after, nil
}

func containsFormat(fs []weavewire.ClipboardFormat, f weavewire.ClipboardFormat) bool {
	for _, x := range fs {
		if x == f {
			return true
		}
	}
	return false
}
