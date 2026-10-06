package weaveclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/internal/cliptransfer"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Streaming clipboard transfer, the host's half: see weavewire's streaming
// transfer for the protocol. Use it with a module whose ClipboardStat reports
// Streaming; an older module answers none of it.

// errGuestDropped is a staged item the guest dropped as it arrived.
var errGuestDropped = errors.New("the guest dropped the item")

// ErrNoSpace matches a fetch refused because the host's disk has no room for
// the file and the reserve. Its errors.As target, *TransferError, carries
// the reason too.
var ErrNoSpace = cliptransfer.ErrNoSpace

// DefaultTransferIdle is how long a streaming transfer waits on a guest that
// moves nothing before it is abandoned.
const DefaultTransferIdle = time.Minute

// TransferOptions tune one streaming transfer.
type TransferOptions struct {
	// Pace, when set, is waited on with each chunk's size before the chunk
	// is sent (host to guest) or credited (guest to host): the bandwidth
	// policy. An error from it stops the transfer.
	Pace func(ctx context.Context, n int) error
	// Progress, when set, is told of every chunk moved, with its size. It
	// runs on the transfer's goroutine and must not block.
	Progress func(n int64)
	// Idle is how long the guest may move nothing before the transfer is
	// abandoned. Zero means DefaultTransferIdle.
	Idle time.Duration
	// Reserve is the free space a fetch keeps on the host's disk beyond the
	// file. Zero means 256 MiB; negative keeps none.
	Reserve int64
	// BeforeSet, when set, sees a streaming set's items just before the set
	// that publishes them, with the items dropped on the way, and returns the
	// items to set: the place to rebuild or remove a companion that names a
	// dropped file (a file copy's name text), so the name never crosses. An
	// item it leaves out is not set; with none left, no set is made. It may
	// change an inline item's Data; a staged one (Stream set) can only be
	// kept or left out.
	BeforeSet func(items []weavewire.ClipboardItem, dropped []ClipboardDrop) []weavewire.ClipboardItem
}

func (o TransferOptions) idle() time.Duration {
	if o.Idle == 0 {
		return DefaultTransferIdle
	}
	return o.Idle
}

func (o TransferOptions) reserve() int64 {
	switch {
	case o.Reserve == 0:
		return cliptransfer.Reserve
	case o.Reserve < 0:
		return 0
	}
	return o.Reserve
}

func (o TransferOptions) progress(n int64) {
	if o.Progress != nil && n > 0 {
		o.Progress(n)
	}
}

// TransferError is one item of a streaming transfer that did not cross:
// Reason is one of the weavewire.ClipboardReason values.
type TransferError struct {
	Reason string
	Err    error
}

func (e *TransferError) Error() string {
	return "weaveclient: clipboard item not transferred (" + e.Reason + "): " + e.Err.Error()
}

func (e *TransferError) Unwrap() error { return e.Err }

// itemError is err as a TransferError, its reason taken from a
// cliptransfer.Failure when it is one.
func itemError(err error, fallback string) error {
	var te *TransferError
	if errors.As(err, &te) {
		return err
	}
	return &TransferError{Reason: cliptransfer.ReasonOf(err, fallback), Err: err}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ClipboardCancel abandons a streaming transfer: the guest stops whatever of it
// is in flight and deletes what it staged. Best effort; a guest that does not
// hear it supersedes the transfer with the next one anyway.
func (c *Client) ClipboardCancel(transferID string) error {
	return c.Notify(
		weavewire.KindClipboardCancel,
		weavewire.ClipboardCancel{TransferID: transferID},
	)
}

// creditDownload acknowledges a streaming get's download as its bytes arrive,
// every ClipboardCreditBytes, paced by opts.Pace. The acknowledgements are
// written from a goroutine of their own: the read loop that receives the bytes
// must never block on a write.
func (c *Client) creditDownload(
	ctx context.Context,
	id string,
	d *download,
	opts TransferOptions,
) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var acked int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-d.done:
				return
			case <-d.credit:
			}
			d.mu.Lock()
			got := int64(d.buf.Len())
			d.mu.Unlock()
			if got-acked < weavewire.ClipboardCreditBytes {
				continue
			}
			if opts.Pace != nil && opts.Pace(ctx, int(got-acked)) != nil {
				return
			}
			opts.progress(got - acked)
			acked = got
			_ = c.Notify(weavewire.KindClipboardCredit, weavewire.ClipboardCredit{
				StreamID: id, Acked: acked,
			})
		}
	}()
	return func() { cancel(); <-done }
}

// fetch is one fetched file's stream, as the read loop hands it over.
type fetch struct {
	chunks chan weavewire.Chunk
	// overrun is closed when the guest sent more than its window allows:
	// the chunks queue is full, and a protocol it does not keep is not one
	// whose bytes can be trusted.
	overrun chan struct{}
	over    bool
}

func (f *fetch) accept(ch weavewire.Chunk) {
	if f.over {
		return
	}
	select {
	case f.chunks <- ch:
	default:
		f.over = true
		close(f.overrun)
	}
}

// FetchedFile is a file of a streaming get, whole on the host's disk.
type FetchedFile struct {
	// Path is where it is: dir/name, as ClipboardFetch was asked.
	Path string
	Size int64
	// SHA256 is its hex digest, checked against the guest's.
	SHA256 string
}

// ClipboardFetch streams one Deferred file of a streaming get — the item at
// index in the reply's Items, of size bytes — to dir/name on the host's disk.
// The bytes are written to dir/name.part as they arrive, and it takes its name
// only once all of them have, at the size declared and with the guest's
// SHA-256; a partial file is removed. The disk must have room for the file
// and opts.Reserve, checked before anything is asked of the guest and again
// as the file is written.
//
// An item that does not cross is a *TransferError with its reason
// (weavewire.ClipboardReasonNoSpace, Integrity or Unreadable). Ending ctx
// abandons the fetch; ClipboardCancel tells the guest.
func (c *Client) ClipboardFetch(
	ctx context.Context,
	transferID string,
	index int,
	size int64,
	dir, name string,
	opts TransferOptions,
) (FetchedFile, error) {
	sink, err := cliptransfer.NewSink(dir, name, size, opts.reserve())
	if err != nil {
		return FetchedFile{}, itemError(err, weavewire.ClipboardReasonNoSpace)
	}
	defer sink.Abort()

	id := c.mintTransfer()
	f := &fetch{
		chunks: make(
			chan weavewire.Chunk,
			weavewire.ClipboardWindowBytes/weavewire.MaxChunkBytes+8,
		),
		overrun: make(chan struct{}),
	}
	c.mu.Lock()
	if c.fetches == nil {
		c.fetches = make(map[string]*fetch)
	}
	c.fetches[id] = f
	c.mu.Unlock()
	c.installClipboardHandlers()
	defer func() {
		c.mu.Lock()
		delete(c.fetches, id)
		c.mu.Unlock()
	}()

	var resp weavewire.ClipboardFetchResponse
	if err := c.Call(ctx, weavewire.KindClipboardFetch, weavewire.ClipboardFetchRequest{
		TransferID: transferID, Index: index, StreamID: id,
	}, &resp); err != nil {
		var ge *GuestError
		if errors.As(err, &ge) {
			return FetchedFile{}, &TransferError{
				Reason: weavewire.ClipboardReasonUnreadable,
				Err:    err,
			}
		}
		return FetchedFile{}, err
	}
	if resp.Size != size {
		_ = c.ClipboardCancel(transferID)
		return FetchedFile{}, &TransferError{
			Reason: weavewire.ClipboardReasonUnreadable,
			Err: fmt.Errorf("%w: the guest has %d bytes of a file it listed at %d",
				cliptransfer.ErrSourceChanged, resp.Size, size),
		}
	}

	idle := opts.idle()
	timer := time.NewTimer(idle)
	defer timer.Stop()
	var acked int64
	for {
		select {
		case ch := <-f.chunks:
			before := sink.Written()
			whole, err := sink.Accept(ch)
			if err != nil {
				return FetchedFile{}, itemError(err, weavewire.ClipboardReasonIntegrity)
			}
			opts.progress(sink.Written() - before)
			if whole {
				return FetchedFile{Path: sink.Path(), Size: size, SHA256: sink.Digest()}, nil
			}
			if n := sink.Written(); n-acked >= weavewire.ClipboardCreditBytes {
				if opts.Pace != nil {
					if err := opts.Pace(ctx, int(n-acked)); err != nil {
						return FetchedFile{}, err
					}
				}
				acked = n
				if err := c.Notify(weavewire.KindClipboardCredit, weavewire.ClipboardCredit{
					StreamID: id, Acked: acked,
				}); err != nil {
					return FetchedFile{}, err
				}
			}
			timer.Reset(idle)
		case <-f.overrun:
			return FetchedFile{}, &TransferError{
				Reason: weavewire.ClipboardReasonIntegrity,
				Err:    fmt.Errorf("%w: the guest sent past its window", cliptransfer.ErrIntegrity),
			}
		case <-timer.C:
			return FetchedFile{}, fmt.Errorf(
				"%w: fetching %s: %w",
				ErrTransfer,
				name,
				cliptransfer.ErrStalled,
			)
		case <-ctx.Done():
			return FetchedFile{}, ctx.Err()
		case <-c.done:
			return FetchedFile{}, c.Err()
		}
	}
}

// ClipboardSource is one item of a streaming set, as the host has it.
type ClipboardSource struct {
	Format weavewire.ClipboardFormat
	// Name is the file's base name, for ClipboardFiles only.
	Name string
	// Data is the item's content in memory, or
	Data []byte
	// Path is a file on the host's disk the item is streamed from, Size
	// bytes long.
	Path string
	Size int64
}

func (s ClipboardSource) size() int64 {
	if s.Path != "" {
		return s.Size
	}
	return int64(len(s.Data))
}

// ClipboardDrop is one item of a streaming set that did not cross.
type ClipboardDrop struct {
	// Index is the item's index in the items sent.
	Index int
	Err   *TransferError
}

// ClipboardSendResult is what a streaming set did.
type ClipboardSendResult struct {
	weavewire.ClipboardSetResponse
	// Set reports that the guest's clipboard was replaced: false when every
	// item was dropped, leaving it as it was.
	Set bool
	// Dropped are the items that did not cross, each with why.
	Dropped []ClipboardDrop
}

// ClipboardSend replaces the guest clipboard with items, every one a
// representation of the same content, streaming whatever does not fit inline:
// each such item is staged on the guest's disk, its bytes paced by the guest's
// acknowledgements and opts.Pace, and verified there by its SHA-256 before the
// set that publishes it. A file streams from its Path and is never held in
// memory here.
//
// Each item is judged on its own: one the guest has no room for, or that does
// not arrive whole, is dropped and listed with its reason, and the rest still
// cross. Only when none is left is no set made. Ending ctx abandons the
// transfer and tells the guest to delete what it staged.
func (c *Client) ClipboardSend(
	ctx context.Context,
	items []ClipboardSource,
	opts TransferOptions,
) (ClipboardSendResult, error) {
	var res ClipboardSendResult
	transfer := c.mintTransfer()
	c.installClipboardHandlers()
	req := weavewire.ClipboardSetRequest{}
	var inline int64
	for i, it := range items {
		item := weavewire.ClipboardItem{Format: it.Format, Name: it.Name, Size: it.size()}
		if it.Path == "" && inline+item.Size <= weavewire.ClipboardInlineBytes {
			inline += item.Size
			item.Data = it.Data
			opts.progress(item.Size)
			req.Items = append(req.Items, item)
			continue
		}
		stream, err := c.stageItem(ctx, transfer, it, opts)
		if err != nil {
			var te *TransferError
			if !errors.As(err, &te) {
				_ = c.ClipboardCancel(transfer)
				return res, err
			}
			res.Dropped = append(res.Dropped, ClipboardDrop{Index: i, Err: te})
			continue
		}
		item.Stream = stream
		req.Items = append(req.Items, item)
	}
	if opts.BeforeSet != nil {
		req.Items = opts.BeforeSet(req.Items, res.Dropped)
		for i := range req.Items {
			if req.Items[i].Stream == "" {
				req.Items[i].Size = int64(len(req.Items[i].Data))
			}
		}
	}
	if len(req.Items) == 0 {
		return res, nil
	}
	if err := c.Call(ctx, weavewire.KindClipboardSet, req, &res.ClipboardSetResponse); err != nil {
		_ = c.ClipboardCancel(transfer)
		return res, err
	}
	res.Set = true
	return res, nil
}

// stageItem stages one item on the guest and streams it there, returning its
// stream id once the guest has it whole. An item that does not cross is a
// *TransferError; any other error ends the whole transfer.
func (c *Client) stageItem(
	ctx context.Context,
	transfer string,
	it ClipboardSource,
	opts TransferOptions,
) (string, error) {
	var r io.Reader = bytes.NewReader(it.Data)
	if it.Path != "" {
		f, err := os.Open(it.Path) //nolint:gosec // G304: a file the user copied
		if err != nil {
			return "", &TransferError{Reason: weavewire.ClipboardReasonUnreadable, Err: err}
		}
		defer f.Close() //nolint:errcheck // read only
		r = f
	}
	id := c.mintTransfer()
	var st weavewire.ClipboardStageResponse
	if err := c.Call(ctx, weavewire.KindClipboardStage, weavewire.ClipboardStageRequest{
		TransferID: transfer, StreamID: id, Format: it.Format, Name: it.Name, Size: it.size(),
	}, &st); err != nil {
		return "", err
	}
	if st.Refused != "" {
		return "", &TransferError{
			Reason: st.Refused,
			Err: fmt.Errorf("%w: the guest refused %d bytes with %d free",
				ErrTransfer, it.size(), st.Free),
		}
	}

	w := cliptransfer.NewWindow()
	c.mu.Lock()
	if c.stages == nil {
		c.stages = make(map[string]*cliptransfer.Window)
	}
	c.stages[id] = w
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.stages, id)
		c.mu.Unlock()
	}()

	var sent int64
	_, err := cliptransfer.Sender{
		Size:   it.size(),
		Window: w,
		Idle:   opts.idle(),
		Pace:   opts.Pace,
		Emit: func(_ context.Context, ch weavewire.Chunk) error {
			ch.StreamID = id
			return c.Notify(weavewire.KindClipboardPut, ch)
		},
		Progress: func(n int64) { opts.progress(n - sent); sent = n },
	}.Stream(ctx, r)
	if err == nil {
		err = w.WaitEnd(ctx, opts.idle())
	}
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", err
	case errors.Is(err, cliptransfer.ErrStalled):
		return "", fmt.Errorf("%w: staging %s: %w", ErrTransfer, it.Format, err)
	}
	var f *cliptransfer.Failure
	if errors.As(err, &f) {
		return "", &TransferError{Reason: f.Reason, Err: f.Err}
	}
	return "", err
}

// routeStaged hands the guest's acknowledgement of a staged item to its window.
func (c *Client) routeStaged(_ string, data []byte) {
	var ev weavewire.ClipboardStaged
	if err := json.Unmarshal(data, &ev); err != nil {
		c.log.Warn("weaveclient: undecodable clipboard acknowledgement", "err", err)
		return
	}
	c.mu.Lock()
	w := c.stages[ev.StreamID]
	c.mu.Unlock()
	if w == nil {
		return
	}
	w.Ack(ev.Acked)
	if !ev.Done {
		return
	}
	if ev.Reason == "" {
		w.End(nil)
		return
	}
	w.End(
		&cliptransfer.Failure{
			Reason: ev.Reason,
			Err:    fmt.Errorf("%w: %s", errGuestDropped, ev.Err),
		},
	)
}
