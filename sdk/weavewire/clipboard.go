package weavewire

// The clipboard is the console user's, so its modules run in that user's
// session (PlacementPerUserConsole), and the capability is a mechanism only:
// read, write and detect change. Which direction may flow, which formats and
// whether files may cross are the host's policy, decided before it asks — the
// guest never second-guesses a set or filters a get beyond what it was asked
// for.
//
// There is no change event. The host polls stat, which carries a change
// token and no data, at the cadence its sync loop wants. A module-side watch
// would itself be a poll on two of the three OSes (macOS has no pasteboard
// notification for a background process, and Linux has no change counter at
// all — see ChangeToken), and it would push events at a host that may not be
// syncing.

const (
	// KindClipboardStat reports the change token and the formats on offer,
	// without their data — the cheap question a sync loop asks every cycle.
	KindClipboardStat = "weave.clipboard.stat"
	// KindClipboardGet reads representations. Content larger than
	// ClipboardInlineBytes follows the reply as KindClipboardDownload chunks.
	KindClipboardGet = "weave.clipboard.get"
	// KindClipboardSet replaces the clipboard with one or more
	// representations of the same content.
	KindClipboardSet = "weave.clipboard.set"
	// KindClipboardUpload carries a Chunk of a set's content ahead of the set
	// that applies it. Sent WITHOUT a correlation id, like exec stdin, and
	// applied in order (IsOrderedInbound), so the set that follows the
	// chunks finds them all already received.
	KindClipboardUpload = "weave.clipboard.upload"
)

// KindClipboardDownload is the guest-to-host event carrying a Chunk of a get's
// content, under the get's TransferID. No correlation id: it answers nothing.
const KindClipboardDownload = "weave.clipboard.download"

// Size limits.
const (
	// ClipboardInlineBytes is the most content a get reply or a set request
	// carries inside itself. Above it the bytes travel as chunks: a reply is
	// one gRPC message between module and core, and base64 inflates it by a
	// third, so this stays an order of magnitude under gRPC's 4 MiB default
	// for the same reason MaxChunkBytes does.
	ClipboardInlineBytes = 256 << 10
	// MaxClipboardBytes caps one get or set in total. A clipboard is a
	// desktop convenience, not a file transfer, and a guest holding a whole
	// transfer in memory while it reassembles it needs a ceiling.
	MaxClipboardBytes = 64 << 20
)

// ClipboardFormat names one representation of clipboard content. The values
// are MIME-shaped and OS-neutral: each backend translates them to its native
// types (UTIs on macOS, X11 and Wayland targets on Linux, CF_* and registered
// formats on Windows).
type ClipboardFormat string

// The canonical formats: the one vocabulary every guest OS speaks. Every
// backend maps each of them to a native name and holds all of them; one it
// cannot hold is reported in ClipboardStatResponse.Support, never dropped
// silently. A host may send other formats; a set leaves them out and lists
// them in ClipboardSetResponse.Unwritten.
const (
	// ClipboardText is UTF-8 text, whatever the OS's native encoding.
	ClipboardText ClipboardFormat = "text/plain"
	ClipboardHTML ClipboardFormat = "text/html"
	ClipboardRTF  ClipboardFormat = "text/rtf"
	ClipboardPNG  ClipboardFormat = "image/png"
	ClipboardTIFF ClipboardFormat = "image/tiff"
	ClipboardPDF  ClipboardFormat = "application/pdf"
	// ClipboardFiles is a copied set of files: one ClipboardItem per file,
	// its Name the file's base name and its data the file's contents. The
	// guest stages received files and puts their paths on its clipboard, so a
	// paste in the guest copies real files rather than a list of host paths
	// that do not exist there.
	ClipboardFiles ClipboardFormat = "files"
)

// ClipboardFormats lists the canonical formats, richest first, so a host that
// wants one representation can take the first it supports, and a backend that
// holds only one per set keeps the richest.
func ClipboardFormats() []ClipboardFormat {
	return []ClipboardFormat{
		ClipboardFiles, ClipboardPNG, ClipboardTIFF, ClipboardPDF,
		ClipboardRTF, ClipboardHTML, ClipboardText,
	}
}

// IsClipboardFormat reports whether f is one of the canonical formats.
func IsClipboardFormat(f ClipboardFormat) bool {
	switch f {
	case ClipboardText, ClipboardHTML, ClipboardRTF, ClipboardPNG,
		ClipboardTIFF, ClipboardPDF, ClipboardFiles:
		return true
	}
	return false
}

// ClipboardFormatSupport says how a guest holds one canonical format.
type ClipboardFormatSupport struct {
	Format ClipboardFormat `json:"format"`
	// Held reports that the guest can hold the format. When it cannot, a set
	// leaves it out and lists it in Unwritten, and Reason says why.
	Held bool `json:"held"`
	// Native is the OS's name for the format: a UTI on macOS, an X11 target
	// or MIME type on Linux, a clipboard format name on Windows.
	Native string `json:"native,omitempty"`
	// Private marks a format the OS has no shared slot for, held under a
	// name of weave's own (PDF on Windows). It round-trips host to guest to
	// host, but most guest applications will neither paste nor copy it.
	Private bool   `json:"private,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ClipboardFormatInfo is one format on offer, as stat reports it.
type ClipboardFormatInfo struct {
	Format ClipboardFormat `json:"format"`
	// Size is the representation's byte length (for files, their total), or
	// 0 when the OS cannot tell without reading the data.
	Size int64 `json:"size,omitempty"`
	// Count is the number of files, for ClipboardFiles only.
	Count int `json:"count,omitempty"`
}

// ClipboardStatResponse is what stat reports.
type ClipboardStatResponse struct {
	// ChangeToken changes whenever the clipboard's content does. Compare it
	// for equality only: it is the OS's change counter on macOS and Windows,
	// but a digest of the content on Linux, which has no counter — so it is
	// neither monotonic nor comparable across a module restart (a new console
	// session starts a new module).
	ChangeToken uint64                `json:"change_token"`
	Formats     []ClipboardFormatInfo `json:"formats,omitempty"`
	// Support lists every canonical format once, in ClipboardFormats order,
	// with whether and how this guest holds it: what a set can carry, not
	// what the clipboard holds now. Empty from a module older than the field.
	Support []ClipboardFormatSupport `json:"support,omitempty"`
	// SingleRepresentation reports a clipboard that holds one representation
	// per set (Linux through wl-copy, on a compositor without a data-control
	// protocol): a set keeps the richest it can hold and lists the rest in
	// Unwritten. Limitation says why.
	SingleRepresentation bool `json:"single_representation,omitempty"`
	// Limitation describes, for an operator, anything that keeps this
	// guest's clipboard from holding every canonical format at once.
	Limitation string `json:"limitation,omitempty"`
}

// ClipboardItem is one representation: in a get's reply, one the guest read;
// in a set, one to write.
type ClipboardItem struct {
	Format ClipboardFormat `json:"format"`
	// Name is the file's base name, for ClipboardFiles only.
	Name string `json:"name,omitempty"`
	// Size is the length of the representation's data, whether it travels
	// inline or as chunks.
	Size int64 `json:"size"`
	// Data is the content when it travels inline; empty when it is streamed.
	Data []byte `json:"data,omitempty"`
}

// ClipboardGetRequest asks for the clipboard's content.
type ClipboardGetRequest struct {
	// Formats are the representations wanted; empty asks for every one the
	// guest has. This is the host's format policy, applied before anything
	// is read.
	Formats []ClipboardFormat `json:"formats,omitempty"`
	// MaxBytes caps each representation (each file, for ClipboardFiles).
	// One over it is left out and listed in Omitted rather than truncated —
	// half an image is not an image. Zero or anything above
	// MaxClipboardBytes means MaxClipboardBytes.
	MaxBytes int64 `json:"max_bytes,omitempty"`
	// TransferID is minted by the HOST and names the chunk stream that
	// carries content over ClipboardInlineBytes. The host must know it
	// before the reply arrives, because the chunks follow the reply down the
	// same ordered channel (see ExecRequest.ExecID). Without one, content
	// over the inline limit is refused.
	TransferID string `json:"transfer_id,omitempty"`
}

// ClipboardGetResponse is one consistent read of the clipboard.
type ClipboardGetResponse struct {
	// ChangeToken is the token of the content read, so a host can tell the
	// clipboard changed while it was reading.
	ChangeToken uint64          `json:"change_token"`
	Items       []ClipboardItem `json:"items,omitempty"`
	// Omitted lists representations over the size cap, with their Size and
	// no Data, so a host can report what it did not sync.
	Omitted []ClipboardItem `json:"omitted,omitempty"`
	// Streamed reports that the Items' data follows as KindClipboardDownload
	// chunks under the request's TransferID: every item's bytes,
	// concatenated in Items order, ending with an EOF chunk.
	Streamed bool `json:"streamed,omitempty"`
}

// ClipboardSetRequest replaces the clipboard. Every item is a representation
// of the same content (text and its HTML, say); together they become one
// clipboard entry, or a backend that holds only one representation at a time
// (ClipboardStatResponse.SingleRepresentation) writes the richest it can. A
// set none of whose formats the guest can hold is refused as unsupported
// (CodeUnsupported) and leaves the clipboard as it was.
type ClipboardSetRequest struct {
	Items []ClipboardItem `json:"items"`
	// TransferID names content uploaded beforehand as KindClipboardUpload
	// chunks: every item's bytes, concatenated in Items order. Each item
	// then carries its Size and no Data. Empty means the data is inline,
	// which it may only be up to ClipboardInlineBytes in total.
	TransferID string `json:"transfer_id,omitempty"`
}

// ClipboardSetResponse reports what the guest's clipboard now holds.
type ClipboardSetResponse struct {
	// ChangeToken is the clipboard's token after the write. The host records
	// it so its next stat does not mistake its own write for a guest change
	// and copy it straight back.
	ChangeToken uint64 `json:"change_token"`
	// Written are the formats the OS accepted, which may be fewer than were
	// sent.
	Written []ClipboardFormat `json:"written,omitempty"`
	// Unwritten are the formats that were sent and not written, each once,
	// in the order they were sent: a format the guest does not hold, or one
	// a single-representation clipboard dropped for a richer one. A host
	// reports them rather than assume the guest holds what it sent.
	Unwritten []ClipboardFormat `json:"unwritten,omitempty"`
}
