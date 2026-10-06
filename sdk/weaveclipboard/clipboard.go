// Package weaveclipboard is the clipboard capability: report what the console
// user's clipboard holds, read it, and replace it. Content of any size crosses
// to a host that streams (weavewire.ClipboardStatResponse.Streaming): files
// stream from and to disk, never held whole in memory, each verified by its
// SHA-256 before it is published. A host that does not stream gets the older
// transfer, inline or as chunk streams of at most weavewire.MaxClipboardBytes
// in all.
//
// It is mechanism only. Which direction may flow, which formats and whether
// files may cross are decided by the host before it asks; the service applies
// the formats and size cap it is given and nothing else.
package weaveclipboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/internal/cliptransfer"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveagent"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Backend is the console user's clipboard on one OS. Each per-OS module
// supplies one; the module runs in that user's session, so a backend talks to
// the clipboard directly.
type Backend interface {
	// Stat reports the change token and the formats on offer without
	// reading their data.
	Stat(ctx context.Context) (weavewire.ClipboardStatResponse, error)
	// Read returns the representations in formats (all it has when formats
	// is empty), with their data. A representation larger than maxBytes may
	// be returned with its Size and no Data rather than read; the service
	// leaves those out of the reply and lists them as omitted. Files are
	// best returned by path in Contents.Files, unread (FilesAt): the
	// service then streams them, and reads one into memory only for a host
	// too old to stream.
	Read(ctx context.Context, formats []weavewire.ClipboardFormat, maxBytes int64) (Contents, error)
	// Write replaces the clipboard with items, every one a representation
	// of the same content, and reports the token afterwards and the formats
	// the OS took.
	Write(
		ctx context.Context,
		items []weavewire.ClipboardItem,
	) (weavewire.ClipboardSetResponse, error)
}

// Support is what a backend's clipboard can hold: for every canonical format
// (weavewire.ClipboardFormats), whether and under which native name.
type Support struct {
	Formats []weavewire.ClipboardFormatSupport
	// SingleRepresentation reports a clipboard that holds one representation
	// per set; Limitation says why.
	SingleRepresentation bool
	Limitation           string
}

// Describer is a Backend that says what its clipboard can hold. The service
// reports it in every stat, refuses a set of nothing it can hold, and passes
// a set only the formats it holds. A backend that does not describe itself is
// taken to hold every canonical format.
type Describer interface {
	Support() Support
}

// CanonicalSupport is the Support of a backend that holds every canonical
// format at once, with native names from natives.
func CanonicalSupport(natives map[weavewire.ClipboardFormat]string) Support {
	var s Support
	for _, f := range weavewire.ClipboardFormats() {
		s.Formats = append(s.Formats, weavewire.ClipboardFormatSupport{
			Format: f, Held: true, Native: natives[f],
		})
	}
	return s
}

// Contents is one consistent read of the clipboard.
type Contents struct {
	ChangeToken uint64
	Items       []weavewire.ClipboardItem
	// Files are copied files offered by path, sized and not read.
	Files []File
}

// Errors a host can branch on, through the guest error's text.
var (
	// ErrTooLarge: content over weavewire.MaxClipboardBytes, or over
	// weavewire.ClipboardInlineBytes without a transfer id to stream it.
	ErrTooLarge = errors.New("weaveclipboard: content too large")
	// ErrBadRequest: a request that does not describe content consistently.
	ErrBadRequest = errors.New("weaveclipboard: bad request")
	// ErrUpload: a set's uploaded content is missing, incomplete or broken.
	ErrUpload = errors.New("weaveclipboard: upload")
)

// maxPendingUploads bounds the uploads held for a set that has not arrived.
// A host uploads and then sets at once, so more than one in flight is a host
// that died between the two; the oldest is dropped rather than letting
// abandoned uploads hold MaxClipboardBytes each for the life of the module.
const maxPendingUploads = 2

// Service is the clipboard capability over an OS backend.
type Service struct {
	b    Backend
	emit weaveagent.Emitter

	mu      sync.Mutex
	uploads map[string]*upload
	order   []string

	// Streaming transfer. rootDir is where staging goes (a new temporary
	// directory when empty) and root the directory in use; dirs numbers the
	// transfer directories under it. offer is the newest streaming get,
	// windows the streams to the host by id, inbound the set transfer being
	// staged and published the directory of the files on the clipboard now.
	// reserve is the free space a stage keeps on the disk.
	rootDir   string
	root      string
	dirs      int
	offer     *offer
	windows   map[string]*cliptransfer.Window
	inbound   *staging
	published string
	reserve   int64
	// cancelled is the transfer last cancelled, and stopped a module that
	// has stopped: neither stages anything more.
	cancelled string
	stopped   bool

	// sizes holds the sizes stat measured for the content at sizesToken, so
	// a host polling an unchanged clipboard costs no read. Guarded by sizesMu,
	// apart from mu so a stat never waits behind an upload.
	sizesMu    sync.Mutex
	sizes      map[weavewire.ClipboardFormat]int64
	sizesToken uint64
}

// Option configures a Service.
type Option func(*Service)

// WithStagingDir stages the files a host sends under dir rather than a new
// temporary directory.
func WithStagingDir(dir string) Option { return func(s *Service) { s.rootDir = dir } }

// NewService builds the clipboard service.
func NewService(b Backend, opts ...Option) *Service {
	s := &Service{
		b:       b,
		uploads: make(map[string]*upload),
		windows: make(map[string]*cliptransfer.Window),
		reserve: cliptransfer.Reserve,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start removes what earlier runs left in the staging directory: partial files of
// transfers that died with them, and run directories long abandoned. It looks
// wherever staging may have gone and creates nothing.
func (s *Service) Start(context.Context) error {
	bases := []string{s.rootDir}
	if s.rootDir == "" {
		bases = append(stagingCandidates(), fallbackStaging())
	}
	now := time.Now()
	for _, b := range bases {
		cleanStale(b, now)
	}
	return nil
}

// Stop ends every transfer in flight and deletes what it staged. The files on
// the clipboard stay: a paste may still want them.
func (s *Service) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	s.dropOffer()
	s.dropStaging()
	return nil
}

// Capability implements weavemodule.Service.
func (s *Service) Capability() weavewire.Capability { return weavewire.Clipboard }

// Register implements weavemodule.Service.
func (s *Service) Register(r *weavemodule.Registrar) error {
	s.emit = r.Emitter()
	r.Handle(weavewire.KindClipboardStat, s.handleStat)
	r.HandleDeferred(weavewire.KindClipboardGet, s.handleGet)
	r.Handle(weavewire.KindClipboardSet, s.handleSet)
	r.Handle(weavewire.KindClipboardUpload, s.handleUpload)
	r.HandleDeferred(weavewire.KindClipboardFetch, s.handleFetch)
	r.Handle(weavewire.KindClipboardStage, s.handleStage)
	r.Handle(weavewire.KindClipboardPut, s.handlePut)
	r.Handle(weavewire.KindClipboardCredit, s.handleCredit)
	r.Handle(weavewire.KindClipboardCancel, s.handleCancel)
	return nil
}

// support is what the backend holds.
func (s *Service) support() Support {
	if d, ok := s.b.(Describer); ok {
		return d.Support()
	}
	return CanonicalSupport(nil)
}

func (s *Service) handleStat(ctx context.Context, _ []byte) ([]byte, error) {
	st, err := s.b.Stat(ctx)
	if err != nil {
		return nil, fmt.Errorf("weaveclipboard: stat: %w", err)
	}
	s.measure(ctx, &st)
	sup := s.support()
	st.Support = sup.Formats
	st.SingleRepresentation = sup.SingleRepresentation
	st.Limitation = sup.Limitation
	st.Streaming = true
	return weavewire.EncodePayload(st)
}

// measure fills in the size of every format a backend's stat left at zero,
// by reading it once per change of the token.
//
// A backend leaves a size out when its OS cannot tell without reading the
// data (every format but files, on every OS today). A host auditing a copy it
// will not read (a guest-to-host direction it blocks) has only stat to go on,
// and without this would record the copy as zero bytes. The read is the one a
// get makes, so a size is what a get would carry; it is made once per change,
// never on every poll, and a read that fails leaves the sizes unknown rather
// than failing the stat.
func (s *Service) measure(ctx context.Context, st *weavewire.ClipboardStatResponse) {
	var unknown []weavewire.ClipboardFormat
	for _, f := range st.Formats {
		if f.Size == 0 && f.Format != weavewire.ClipboardFiles {
			unknown = append(unknown, f.Format)
		}
	}
	if len(unknown) == 0 {
		return
	}
	s.sizesMu.Lock()
	defer s.sizesMu.Unlock()
	if s.sizes == nil || s.sizesToken != st.ChangeToken {
		c, err := s.b.Read(ctx, unknown, weavewire.MaxClipboardBytes)
		if err != nil || c.ChangeToken != st.ChangeToken {
			return // unreadable, or changed since the stat: the next poll measures
		}
		s.sizes, s.sizesToken = make(map[weavewire.ClipboardFormat]int64), c.ChangeToken
		for _, it := range c.Items {
			if it.Data != nil {
				it.Size = int64(len(it.Data))
			}
			s.sizes[it.Format] += it.Size
		}
	}
	for i, f := range st.Formats {
		if f.Size == 0 && f.Format != weavewire.ClipboardFiles {
			st.Formats[i].Size = s.sizes[f.Format]
		}
	}
}

// handleGet reads the clipboard and replies with its content: inline when it
// fits, otherwise as a manifest whose bytes follow as download chunks.
//
// The chunks are sent from the deferred half, after the reply is on the wire,
// so the host has the manifest — sizes and order — before the first byte of
// the stream reaches it.
func (s *Service) handleGet(ctx context.Context, payload []byte) ([]byte, func(), error) {
	var req weavewire.ClipboardGetRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, nil, fmt.Errorf("%w: decoding get: %w", ErrBadRequest, err)
		}
	}
	if req.Stream {
		return s.streamingGet(ctx, req)
	}
	limit := req.MaxBytes
	if limit <= 0 || limit > weavewire.MaxClipboardBytes {
		limit = weavewire.MaxClipboardBytes
	}

	c, err := s.b.Read(ctx, req.Formats, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("weaveclipboard: reading: %w", err)
	}
	resp := weavewire.ClipboardGetResponse{ChangeToken: c.ChangeToken}
	var total int64
	for i, it := range append(fileItems(c.Files), c.Items...) {
		if len(req.Formats) > 0 && !slices.Contains(req.Formats, it.Format) {
			continue // the host's format policy holds even if a backend over-reads
		}
		if it.Data != nil {
			it.Size = int64(len(it.Data))
		}
		if i < len(c.Files) && it.Size <= limit && total+it.Size <= weavewire.MaxClipboardBytes {
			it.Data = readFile(c.Files[i]) // a file by path, in bounds: read it for this host
		}
		// Over the per-item cap, over what the whole get may carry, or
		// not read by the backend: listed, never truncated.
		if it.Size > limit || total+it.Size > weavewire.MaxClipboardBytes ||
			(it.Data == nil && it.Size > 0) {
			resp.Omitted = append(resp.Omitted, weavewire.ClipboardItem{
				Format: it.Format, Name: it.Name, Size: it.Size,
			})
			continue
		}
		total += it.Size
		resp.Items = append(resp.Items, it)
	}

	if total <= weavewire.ClipboardInlineBytes {
		out, err := weavewire.EncodePayload(resp)
		return out, nil, err
	}
	if req.TransferID == "" {
		return nil, nil, fmt.Errorf(
			"%w: %d bytes is over the %d-byte inline limit and no transfer id was given",
			ErrTooLarge,
			total,
			weavewire.ClipboardInlineBytes,
		)
	}

	resp.Streamed = true
	data := make([][]byte, len(resp.Items))
	for i := range resp.Items {
		data[i] = resp.Items[i].Data
		resp.Items[i].Data = nil
	}
	out, err := weavewire.EncodePayload(resp)
	if err != nil {
		return nil, nil, err
	}
	after := func() {
		w := weaveagent.NewStreamWriter(s.emit, weavewire.KindClipboardDownload, req.TransferID)
		var werr error
		for _, d := range data {
			if _, werr = w.WriteContext(ctx, d); werr != nil {
				break
			}
		}
		// The EOF carries a failure partway, so the host never takes a
		// short stream for the whole clipboard.
		_ = w.Close(ctx, werr)
	}
	return out, after, nil
}

// handleSet replaces the clipboard with inline content or a finished upload.
//
// It runs in the module's ordered queue behind the upload chunks the host sent
// first (weavewire.IsOrderedInbound), so an upload it names is complete unless
// chunks were lost — which the stream's sequence numbers catch.
func (s *Service) handleSet(ctx context.Context, payload []byte) ([]byte, error) {
	var req weavewire.ClipboardSetRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("%w: decoding set: %w", ErrBadRequest, err)
	}
	if len(req.Items) == 0 {
		return nil, fmt.Errorf("%w: a set needs at least one item", ErrBadRequest)
	}
	// Staged items streamed to disk ahead of the set, and have no ceiling;
	// the rest came inline or as an upload held in memory, which does.
	var total int64
	for _, it := range req.Items {
		if it.Format == "" {
			return nil, fmt.Errorf("%w: an item has no format", ErrBadRequest)
		}
		if it.Size < 0 {
			return nil, fmt.Errorf("%w: %s has a negative size", ErrBadRequest, it.Format)
		}
		if it.Stream == "" {
			total += it.Size
		}
	}
	if total > weavewire.MaxClipboardBytes {
		return nil, fmt.Errorf("%w: %d bytes is over the %d-byte limit",
			ErrTooLarge, total, weavewire.MaxClipboardBytes)
	}

	items := slices.Clone(req.Items)
	carried := slices.DeleteFunc(slices.Clone(items), func(it weavewire.ClipboardItem) bool {
		return it.Stream != ""
	})
	if req.TransferID == "" {
		if err := checkInline(carried); err != nil {
			return nil, err
		}
	} else if err := s.attachUpload(req.TransferID, carried, total); err != nil {
		return nil, err
	}
	for i, j := 0, 0; i < len(items); i++ {
		if items[i].Stream == "" {
			items[i] = carried[j]
			j++
		}
	}

	// Only what the guest holds reaches the backend. A set of nothing it
	// holds is an answer, not a fault, and must not empty the clipboard on
	// its way to writing nothing.
	held := make(map[weavewire.ClipboardFormat]bool)
	for _, f := range s.support().Formats {
		held[f.Format] = f.Held
	}
	writable := slices.DeleteFunc(items, func(it weavewire.ClipboardItem) bool {
		return !held[it.Format]
	})
	if len(writable) == 0 {
		return nil, &weavewire.UnsupportedError{
			Kind:   weavewire.KindClipboardSet,
			Reason: "this guest's clipboard holds none of " + formatList(req.Items),
		}
	}

	res, err := s.write(ctx, writable)
	if err != nil {
		return nil, err
	}
	res.Unwritten = nil
	for _, it := range req.Items {
		if !slices.Contains(res.Written, it.Format) && !slices.Contains(res.Unwritten, it.Format) {
			res.Unwritten = append(res.Unwritten, it.Format)
		}
	}
	return weavewire.EncodePayload(res)
}

// formatList names the distinct formats of items, in order.
func formatList(items []weavewire.ClipboardItem) string {
	var names []string
	for _, it := range items {
		if !slices.Contains(names, string(it.Format)) {
			names = append(names, string(it.Format))
		}
	}
	return strings.Join(names, ", ")
}

// checkInline verifies each inline item's declared size against its data and
// the inline limit.
func checkInline(items []weavewire.ClipboardItem) error {
	var total int64
	for i := range items {
		n := int64(len(items[i].Data))
		if items[i].Size != 0 && items[i].Size != n {
			return fmt.Errorf("%w: %s declares %d bytes and carries %d",
				ErrBadRequest, items[i].Format, items[i].Size, n)
		}
		items[i].Size = n
		total += n
	}
	if total > weavewire.ClipboardInlineBytes {
		return fmt.Errorf("%w: %d bytes inline is over the %d-byte inline limit; upload it",
			ErrTooLarge, total, weavewire.ClipboardInlineBytes)
	}
	return nil
}

// attachUpload takes the upload named id and splits it into items, in order,
// by their declared sizes.
func (s *Service) attachUpload(id string, items []weavewire.ClipboardItem, total int64) error {
	s.mu.Lock()
	u := s.uploads[id]
	s.forget(id)
	s.mu.Unlock()

	switch {
	case u == nil:
		return fmt.Errorf("%w: nothing was uploaded as %q", ErrUpload, id)
	case u.err != nil:
		return u.err
	case !u.asm.Done():
		return fmt.Errorf("%w: %q was not finished — chunks were lost or the host stopped sending",
			ErrUpload, id)
	}
	if err := u.asm.Err(); err != nil {
		return fmt.Errorf("%w: %q: %w", ErrUpload, id, err)
	}
	data := u.buf.Bytes()
	if int64(len(data)) != total {
		return fmt.Errorf("%w: %q carried %d bytes and the items declare %d",
			ErrUpload, id, len(data), total)
	}
	for i := range items {
		items[i].Data, data = data[:items[i].Size:items[i].Size], data[items[i].Size:]
	}
	return nil
}

// upload is the host→guest half of a set larger than the inline limit.
type upload struct {
	asm weavewire.StreamAssembler
	buf bytes.Buffer
	err error // the first failure; later chunks are dropped
}

// handleUpload takes one chunk of an upload. It has no reply — the chunks are
// notifications — so a failure is recorded on the upload and reported by the
// set that claims it.
func (s *Service) handleUpload(_ context.Context, payload []byte) ([]byte, error) {
	var c weavewire.Chunk
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("%w: decoding upload chunk: %w", ErrUpload, err)
	}
	if c.StreamID == "" {
		return nil, fmt.Errorf("%w: a chunk with no transfer id", ErrUpload)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[c.StreamID]
	if u == nil {
		u = &upload{}
		s.uploads[c.StreamID] = u
		s.order = append(s.order, c.StreamID)
		for len(s.order) > maxPendingUploads {
			s.forget(s.order[0])
		}
	}
	if u.err != nil {
		return nil, u.err
	}
	data, err := u.asm.Accept(c)
	switch {
	case err != nil:
		u.err = fmt.Errorf("%w: %q: %w", ErrUpload, c.StreamID, err)
	case int64(u.buf.Len()+len(data)) > weavewire.MaxClipboardBytes:
		u.err = fmt.Errorf("%w: %q is over the %d-byte limit",
			ErrTooLarge, c.StreamID, weavewire.MaxClipboardBytes)
	default:
		u.buf.Write(data)
		return nil, nil
	}
	// Drop what was received: the set that claims this upload fails anyway,
	// and the bytes should not sit in memory until it does.
	u.buf = bytes.Buffer{}
	return nil, u.err
}

// forget drops an upload. The caller holds mu.
func (s *Service) forget(id string) {
	delete(s.uploads, id)
	s.order = slices.DeleteFunc(s.order, func(o string) bool { return o == id })
}

// fileItems is files as the items an older host's get carries, sized and not
// yet read.
func fileItems(files []File) []weavewire.ClipboardItem {
	out := make([]weavewire.ClipboardItem, 0, len(files))
	for _, f := range files {
		out = append(out, weavewire.ClipboardItem{
			Format: weavewire.ClipboardFiles, Name: f.Name, Size: f.Size,
		})
	}
	return out
}

// readFile reads a file offered by path, for a host that does not stream. A
// file that cannot be read whole at its size reads as nil: it is then listed
// as omitted, sized and without data.
func readFile(f File) []byte {
	data, err := os.ReadFile(f.Path)
	if err != nil || int64(len(data)) != f.Size {
		return nil
	}
	if data == nil {
		data = []byte{} // an empty file is content, not an unread one
	}
	return data
}

// write puts a set's writable items on the clipboard. A backend that takes
// staged files is handed every file by path: a staged item's from where it
// streamed to, an inline or uploaded one's from where the service writes it.
// A staged item of another format is read back into memory, since the OS's
// clipboard holds it there. Once the clipboard holds the files, the files of
// the copy it held before are deleted.
func (s *Service) write(
	ctx context.Context,
	items []weavewire.ClipboardItem,
) (weavewire.ClipboardSetResponse, error) {
	fw, byPath := s.b.(FileWriter)
	s.mu.Lock()
	dir, staged, err := s.claimStaged(items)
	if err == nil && dir == "" && byPath && slices.ContainsFunc(items, isFile) {
		dir, err = s.newTransferDir()
	}
	if err == nil && s.inbound != nil && s.inbound.dir == dir {
		s.inbound = nil // claimed: no longer a transfer to supersede
	}
	s.mu.Unlock()
	if err != nil {
		return weavewire.ClipboardSetResponse{}, err
	}

	var paths []string
	out := slices.Clone(items)
	for i := range out {
		it := &out[i]
		path, wasStaged := staged[it.Stream]
		file := it.Format == weavewire.ClipboardFiles
		switch {
		case file && byPath && wasStaged:
			paths = append(paths, path)
		case file && byPath:
			p, err := writeStaged(dir, len(paths), it)
			if err != nil {
				s.discard(dir)
				return weavewire.ClipboardSetResponse{}, err
			}
			paths = append(paths, p)
		case wasStaged:
			// The OS's clipboard holds a representation in memory, and a
			// backend that takes no paths holds a file there too.
			data, err := os.ReadFile(path) //nolint:gosec // G304: a staging path of ours
			if err != nil {
				s.discard(dir)
				return weavewire.ClipboardSetResponse{}, fmt.Errorf(
					"weaveclipboard: reading staged %s: %w", it.Format, err)
			}
			it.Data = data
		}
		if file && byPath {
			it.Data = nil
		}
	}

	var res weavewire.ClipboardSetResponse
	if byPath {
		res, err = fw.WriteFiles(ctx, out, paths)
	} else {
		res, err = s.b.Write(ctx, out)
	}
	if err != nil {
		s.discard(dir)
		return res, fmt.Errorf("weaveclipboard: writing: %w", err)
	}
	if dir != "" {
		s.mu.Lock()
		old := s.published
		s.published = dir
		s.mu.Unlock()
		if old != "" && old != dir {
			_ = os.RemoveAll(old)
		}
	}
	return res, nil
}

// discard deletes a transfer directory that will not be published.
func (s *Service) discard(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

func isFile(it weavewire.ClipboardItem) bool { return it.Format == weavewire.ClipboardFiles }

// writeStaged writes an inline or uploaded file into its own numbered
// directory of dir, under its base name.
func writeStaged(dir string, n int, it *weavewire.ClipboardItem) (string, error) {
	name, err := fileName(it.Name)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	sub := filepath.Join(dir, "i"+strconv.Itoa(n))
	if err := os.Mkdir(sub, 0o700); err != nil {
		return "", fmt.Errorf("staging %s: %w", name, err)
	}
	p := filepath.Join(sub, name)
	if err := os.WriteFile(p, it.Data, 0o600); err != nil {
		return "", fmt.Errorf("staging %s: %w", name, err)
	}
	return p, nil
}
