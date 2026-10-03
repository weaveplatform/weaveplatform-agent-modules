package weaveclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// ErrTransfer reports clipboard content that did not arrive whole: a chunk
// lost, a stream that ended early or carried a failure, or one that does not
// match the sizes its reply declared.
var ErrTransfer = errors.New("weaveclient: clipboard transfer failed")

// ClipboardStat reports the guest clipboard's change token and the formats it
// holds, without their data — the question a sync loop asks every cycle.
// There is no change event; poll this at the cadence the sync wants and Get
// when the token moves.
//
// The clipboard runs in the console user's session. With nobody logged in the
// call gets no answer, and fails with ErrNoSession after
// Options.SessionTimeout.
func (c *Client) ClipboardStat(ctx context.Context) (weavewire.ClipboardStatResponse, error) {
	var out weavewire.ClipboardStatResponse
	err := c.Call(ctx, weavewire.KindClipboardStat, nil, &out)
	return out, err
}

// ClipboardGet reads the guest clipboard. req.Formats and req.MaxBytes are
// the host's policy; leave TransferID empty and one is minted.
//
// Every returned item carries its Data, whether it came back inline or as a
// chunk stream; Streamed reports which. Content that does not arrive whole is
// an ErrTransfer, never a short item.
func (c *Client) ClipboardGet(
	ctx context.Context,
	req weavewire.ClipboardGetRequest,
) (weavewire.ClipboardGetResponse, error) {
	if req.TransferID == "" {
		req.TransferID = c.idPrefix + "-clip-" + strconv.FormatUint(c.nextID.Add(1), 10)
	}
	// Registered before the request goes out: the chunks follow the reply
	// down the same ordered channel, and the read loop may route the first of
	// them before this goroutine has even seen the reply.
	d := c.registerDownload(req.TransferID)
	defer c.unregisterDownload(req.TransferID)

	var out weavewire.ClipboardGetResponse
	if err := c.Call(ctx, weavewire.KindClipboardGet, req, &out); err != nil {
		return out, err
	}
	if !out.Streamed {
		return out, nil
	}
	body, err := c.awaitDownload(ctx, d)
	if err != nil {
		return out, err
	}
	var want int64
	for _, it := range out.Items {
		want += it.Size
	}
	if int64(len(body)) != want {
		return out, fmt.Errorf(
			"%w: received %d bytes, the reply declared %d",
			ErrTransfer,
			len(body),
			want,
		)
	}
	for i := range out.Items {
		n := out.Items[i].Size
		out.Items[i].Data, body = body[:n:n], body[n:]
	}
	return out, nil
}

// ClipboardSet replaces the guest clipboard with items, every one a
// representation of the same content. Each item needs its Format and Data;
// Size is filled in. Content over weavewire.ClipboardInlineBytes is uploaded
// as chunks first, transparently.
//
// The response's ChangeToken is the clipboard's token after the write: record
// it, so the next ClipboardStat does not read the host's own write as a guest
// change and copy it straight back.
func (c *Client) ClipboardSet(
	ctx context.Context,
	items []weavewire.ClipboardItem,
) (weavewire.ClipboardSetResponse, error) {
	req := weavewire.ClipboardSetRequest{Items: make([]weavewire.ClipboardItem, len(items))}
	var total int
	for i, it := range items {
		it.Size = int64(len(it.Data))
		total += len(it.Data)
		req.Items[i] = it
	}
	if total > weavewire.MaxClipboardBytes {
		return weavewire.ClipboardSetResponse{}, fmt.Errorf(
			"%w: %d bytes is over the %d-byte limit",
			ErrTransfer,
			total,
			weavewire.MaxClipboardBytes,
		)
	}
	if total > weavewire.ClipboardInlineBytes {
		req.TransferID = c.idPrefix + "-clip-" + strconv.FormatUint(c.nextID.Add(1), 10)
		if err := c.upload(req.TransferID, req.Items); err != nil {
			return weavewire.ClipboardSetResponse{}, err
		}
		for i := range req.Items {
			req.Items[i].Data = nil
		}
	}
	var out weavewire.ClipboardSetResponse
	err := c.Call(ctx, weavewire.KindClipboardSet, req, &out)
	return out, err
}

// upload sends items' bytes as one chunk stream, in order, ending with EOF.
// The set that follows is queued behind these chunks on the guest, so it
// finds them all.
func (c *Client) upload(id string, items []weavewire.ClipboardItem) error {
	var seq uint64
	send := func(ch weavewire.Chunk) error {
		ch.StreamID, ch.Seq = id, seq
		seq++
		return c.Notify(weavewire.KindClipboardUpload, ch)
	}
	for _, it := range items {
		for p := it.Data; len(p) > 0; {
			n := min(len(p), weavewire.MaxChunkBytes)
			if err := send(weavewire.Chunk{Data: p[:n]}); err != nil {
				return err
			}
			p = p[n:]
		}
	}
	return send(weavewire.Chunk{EOF: true})
}

// download collects one get's chunk stream as the read loop routes it.
type download struct {
	mu   sync.Mutex
	asm  weavewire.StreamAssembler
	buf  bytes.Buffer
	err  error
	done chan struct{} // closed at EOF or on the first failure
	// progress is signalled per chunk, so a waiter can tell a slow stream
	// from one whose module went away mid-transfer.
	progress chan struct{}
}

func (d *download) accept(ch weavewire.Chunk) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil || d.asm.Done() {
		return
	}
	data, err := d.asm.Accept(ch)
	switch {
	case err != nil:
		d.err = fmt.Errorf("%w: %w", ErrTransfer, err)
	case d.buf.Len()+len(data) > weavewire.MaxClipboardBytes:
		d.err = fmt.Errorf("%w: over the %d-byte limit", ErrTransfer, weavewire.MaxClipboardBytes)
	default:
		d.buf.Write(data)
		if d.asm.Done() {
			if err := d.asm.Err(); err != nil {
				d.err = fmt.Errorf("%w: %w", ErrTransfer, err)
			}
			close(d.done)
			return
		}
		select {
		case d.progress <- struct{}{}:
		default:
		}
		return
	}
	d.buf = bytes.Buffer{}
	close(d.done)
}

func (c *Client) registerDownload(id string) *download {
	d := &download{done: make(chan struct{}), progress: make(chan struct{}, 1)}
	c.mu.Lock()
	if c.downloads == nil {
		c.downloads = make(map[string]*download)
	}
	c.downloads[id] = d
	install := !c.downloadHandler
	c.downloadHandler = true
	c.mu.Unlock()
	if install {
		c.On(weavewire.KindClipboardDownload, c.routeDownload)
	}
	return d
}

func (c *Client) unregisterDownload(id string) {
	c.mu.Lock()
	delete(c.downloads, id)
	c.mu.Unlock()
}

func (c *Client) routeDownload(_ string, data []byte) {
	var ch weavewire.Chunk
	if err := json.Unmarshal(data, &ch); err != nil {
		c.log.Warn("weaveclient: undecodable clipboard chunk", "err", err)
		return
	}
	c.mu.Lock()
	d := c.downloads[ch.StreamID]
	c.mu.Unlock()
	if d != nil {
		d.accept(ch)
	}
}

// awaitDownload waits for a download to end. Besides the caller's context, a
// stream that stops making progress for Options.SessionTimeout is abandoned
// as ErrNoSession: the clipboard module lives in the console session, and a
// logout mid-transfer stops it with the stream unfinished.
func (c *Client) awaitDownload(ctx context.Context, d *download) ([]byte, error) {
	var stall <-chan time.Time
	var timer *time.Timer
	if c.sessionTimeout > 0 {
		timer = time.NewTimer(c.sessionTimeout)
		defer timer.Stop()
		stall = timer.C
	}
	for {
		select {
		case <-d.done:
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.err != nil {
				return nil, d.err
			}
			return d.buf.Bytes(), nil
		case <-d.progress:
			if timer != nil {
				timer.Reset(c.sessionTimeout)
			}
		case <-stall:
			return nil, fmt.Errorf("%w: the clipboard stream stopped: %w",
				ErrTransfer, noSession(weavewire.KindClipboardGet, weavewire.Clipboard))
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, c.Err()
		}
	}
}
