# Clipboard

The clipboard capability (`weave.clipboard`) behaves the same on every guest OS: the same
ops, the same formats, the same change token, the same errors. A host drives a Linux, macOS
or Windows guest with one engine and one policy.

## Ops

| Op | What it does |
|---|---|
| `weave.clipboard.stat` | The change token, the formats on the clipboard now with their sizes, and what the guest can hold (`support`) |
| `weave.clipboard.get` | The representations asked for (`formats`), each up to `max_bytes`; one over the cap is listed in `omitted` with its size, never truncated |
| `weave.clipboard.set` | Replaces the clipboard with every representation sent, as one entry; lists what it wrote (`written`) and what it did not (`unwritten`) |
| `weave.clipboard.fetch` | Streams one file of a streaming get from the guest's disk |
| `weave.clipboard.stage` | Readies the guest to receive one item of a set, if its disk has room |
| `weave.clipboard.put` | One chunk of a staged item |
| `weave.clipboard.credit` | The host's acknowledgement of a stream's bytes, which lets the guest send more |
| `weave.clipboard.cancel` | Abandons a transfer: what is in flight stops, and what was staged is deleted |

The wire types are in [`sdk/weavewire/clipboard.go`](../sdk/weavewire/clipboard.go).

### Transfer of any size

A module reports `streaming` in every stat. With such a module a copy of any size crosses
in either direction, and no file is ever held whole in memory, on the host, the guest or
core:

- **Guest to host.** The host asks a get with `stream`. Files come back `deferred`: named
  and sized, without data. The host judges each against its own bounds and fetches each
  one it takes (`weave.clipboard.fetch`); its bytes stream from the guest's disk as
  `weave.clipboard.download` chunks, and the host writes them to a partial file as they
  arrive.
- **Host to guest.** The host stages each item too large to carry inline
  (`weave.clipboard.stage`), streams it (`weave.clipboard.put`), and the guest writes it to
  a partial file in its staging directory as it arrives, acknowledging as it goes
  (`weave.clipboard.staged`). The set that follows names each staged item by its stream.
- **Integrity.** Every stream's last chunk carries the SHA-256 of what was sent. A file
  takes its final name, and can be published to a clipboard, only once all of it has
  arrived at the size declared with that digest; anything else is deleted.
- **Flow control.** The receiver acknowledges every 256 KiB written, and the sender keeps
  at most 1 MiB unacknowledged. A transfer of any size therefore holds at most that much
  in any queue on the channel, and never stalls the exec, power and control frames that
  share it. A host paces its sends and acknowledgements to its bandwidth policy.
- **Disk space.** The receiving side refuses an item its disk has no room for, keeping
  256 MiB free beyond it, and checks again every 32 MiB written. A refused item is dropped
  alone, with the reason `no-space`; the rest of the copy still crosses.
- **Supersede and cancel.** A newer transfer supersedes an older one: whatever the older
  one still had in flight stops, and what it staged is deleted. `weave.clipboard.cancel`
  does the same at once. A set's files stay on the guest's disk until a newer set replaces
  them on the clipboard.

Representations other than files live in memory on both sides, since the OS's clipboard
holds them there. They cross inline up to 256 KiB and as a flow-controlled download or a
staged item above it. One larger than 1 GiB is listed in `omitted` with its size, never
truncated.

A host or module from before streaming transfer uses the older path: content over 256 KiB
as `weave.clipboard.upload` and `weave.clipboard.download` streams held in memory, up to
64 MiB in all. A host that meets such a module should leave out what does not fit and
report it, rather than fail the copy.

## Canonical formats

One list, in `weavewire.ClipboardFormats`, richest first:

| Format | macOS (UTI) | Linux (targets offered) | Windows (clipboard format) |
|---|---|---|---|
| `files` | `public.file-url`, one pasteboard item per file | `text/uri-list`, `x-special/gnome-copied-files` | `CF_HDROP` |
| `image/png` | `public.png` | `image/png` | `PNG` (registered); also written as `CF_DIBV5` and `CF_DIB`, and read from them when no `PNG` is there |
| `image/tiff` | `public.tiff` | `image/tiff` | `CF_TIFF` |
| `application/pdf` | `com.adobe.pdf` | `application/pdf` | `Portable Document Format` (registered); reported `private`, since Windows has no PDF format applications share |
| `text/rtf` | `public.rtf` | `text/rtf`, `application/rtf` | `Rich Text Format` (registered) |
| `text/html` | `public.html` | `text/html` | `HTML Format` (registered; the CF_HTML header is added on set and removed on get) |
| `text/plain` | `public.utf8-plain-text` | `text/plain;charset=utf-8`, `UTF8_STRING`, `text/plain` | `CF_UNICODETEXT` (UTF-16; UTF-8 on the wire) |

Windows applications that copy an image as a bitmap alone (Paint, and most older ones)
copy `CF_DIB`, `CF_DIBV5` or `CF_BITMAP` and no PNG. The Windows module offers such a
clipboard as `image/png` and converts the bitmap when it is read: 1, 4, 8, 16, 24 and 32
bits per pixel, `BI_RGB`, `BI_BITFIELDS` and `BI_ALPHABITFIELDS`, rows in either order, and
the PNG inside a `BI_PNG` bitmap as it is. A 32-bit bitmap keeps its alpha unless every
pixel's is zero, which is padding, not transparency. A set's PNG is also written as a
`CF_DIBV5` with its alpha and a 24-bit `CF_DIB` composited over white, so those
applications paste it; reading back returns the PNG as it was sent.

Files cross as their content: the guest stages received files and puts their paths on its
clipboard, so a paste copies real files, and a get offers the files the clipboard names,
read from disk as they stream.

**No silent drops.** Stat's `support` lists every canonical format once, in the order
above, with whether the guest holds it, its native name, or why not. A set leaves out what
the guest cannot hold and lists it in `unwritten`; a set of nothing the guest can hold is
answered `unsupported` and leaves the clipboard as it was. A format outside the list is
never written and always listed in `unwritten`.

A stat's sizes are what a get with no cap would carry, so a host can audit a copy it does
not read (a direction its policy blocks) at its real size. Files are sized from the
filesystem; every other format is read once per change of the token to size it, never on
every poll.

## The change token

Stat, get and set each report a token that changes whenever the clipboard's content
changes, by any application, the guest module's own sets included. Compare it for equality
only.

| OS | Source |
|---|---|
| macOS | the pasteboard's `changeCount` |
| Windows | `GetClipboardSequenceNumber` |
| Linux, X11 | a count of XFixes selection-owner notifications |
| Linux, Wayland data control | a count of the device's selection events |
| Linux, wl-clipboard | a digest of the content and the module's own sets |

## Linux

The module runs in the console user's session and holds the clipboard itself, in pure Go
(CGO stays off), so every representation of a set is held at once:

- **Wayland with data control.** The module binds `ext_data_control_manager_v1`
  (wayland-protocols 1.39), or `zwlr_data_control_manager_v1` where the compositor has only
  that, on the first seat. A set creates a data source offering every MIME type and writes
  each to the pipe the compositor hands it when something pastes; a get receives each type
  through a pipe. No maintained pure-Go Wayland client generates these protocols, so the
  module speaks the Wayland wire protocol itself (`wlwire.go`, `wayland.go`). Compositors
  with one of them: the wlroots ones (Sway, Hyprland, river, labwc, Wayfire, niri, cage),
  KDE Plasma (KWin) and COSMIC.
- **X11.** The module owns the `CLIPBOARD` selection (`github.com/jezek/xgb`): it answers
  `TARGETS`, `TIMESTAMP` and every data target, sending data larger than one request in
  `INCR` chunks, for as long as it owns the selection. It reads other applications'
  selections the same way, `INCR` included. This also covers **GNOME**: Mutter has no data
  control protocol, but runs XWayland and bridges its `CLIPBOARD` to Wayland clients, so on
  a GNOME session with `DISPLAY` set the module holds every representation through X11.
- **Fallback: wl-clipboard.** A Wayland session with neither (no data control and no X
  display) is reached through `wl-paste` and `wl-copy`, which hold one representation per
  copy. Stat then reports `single_representation` with a `limitation`, a set keeps the
  richest format and lists the rest in `unwritten`, and the token is a content digest.

The connection is made at the first op and made again after it breaks (a compositor
restart). X11 and Wayland ownership ends when the module stops, as any application's copy
does when it exits.

## The contract

[`sdk/weaveclipboard/weaveclipboardtest`](../sdk/weaveclipboard/weaveclipboardtest)
`RunContract` is one test of all of the above, driven through the clipboard service as a
host's calls reach it. Each module runs it against its real backend over a clipboard of the
test's own, never the clipboard of the machine running the tests:

| Module | Against |
|---|---|
| `weave-macos-clipboard` | NSPasteboard, on a uniquely named pasteboard per check; skipped outside a GUI session |
| `weave-linux-clipboard` | data control (ext and wlr) on an in-process compositor; X11 on an Xvfb display the tests start (CI installs Xvfb); wl-clipboard through stand-ins; a real compositor when `WEAVE_CLIPBOARD_TEST_WAYLAND_DISPLAY` names one |
| `weave-windows-clipboard` | the real backend over a stand-in for the Win32 clipboard calls (real memory blocks, encodings and `DragQueryFile`); the session's real clipboard only with `WEAVE_CLIPBOARD_CONTRACT_SYSTEM=1`, since it overwrites it |

It checks that the token changes on every set and that stat and get report it; that
stat lists the formats present and the guest's support; that a get honours `formats` and
`max_bytes`; that stat sizes every format it lists; that a set of every canonical format reads back
byte for byte; files (staged, named by base name, size-capped and reported when omitted, a
bad name refused without touching the clipboard); a copy of files some of which are over
the cap, which leaves out only those, wherever they are in the copy, and brings the rest;
content large enough to stream both ways; a file larger than the older path's 64 MiB
ceiling sent to the guest and fetched back through the real client and channel framing,
verified by its SHA-256 (72 MiB by default, or `WEAVE_CLIPBOARD_CONTRACT_LARGE_MIB`; it is
generated in the test's temporary directory, never stored); and the error answers,
`unsupported` included.

`make test-linux-clipboard` runs the Linux module's tests in Docker, as an ordinary user,
against Xvfb, xclip, wl-clipboard and a headless Sway.
