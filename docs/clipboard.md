# Clipboard

The clipboard capability (`weave.clipboard`) behaves the same on every guest OS: the same
ops, the same formats, the same change token, the same errors. A host drives a Linux, macOS
or Windows guest with one engine and one policy.

## Ops

| Op | What it does |
|---|---|
| `weave.clipboard.stat` | The change token, the formats on the clipboard now, and what the guest can hold (`support`) |
| `weave.clipboard.get` | The representations asked for (`formats`), each up to `max_bytes`; one over the cap is listed in `omitted` with its size, never truncated |
| `weave.clipboard.set` | Replaces the clipboard with every representation sent, as one entry; lists what it wrote (`written`) and what it did not (`unwritten`) |

Content over 256 KiB travels as chunk streams (`weave.clipboard.upload`,
`weave.clipboard.download`), up to 64 MiB in all. The wire types are in
[`sdk/weavewire/clipboard.go`](../sdk/weavewire/clipboard.go).

## Canonical formats

One list, in `weavewire.ClipboardFormats`, richest first:

| Format | macOS (UTI) | Linux (targets offered) | Windows (clipboard format) |
|---|---|---|---|
| `files` | `public.file-url`, one pasteboard item per file | `text/uri-list`, `x-special/gnome-copied-files` | `CF_HDROP` |
| `image/png` | `public.png` | `image/png` | `PNG` (registered) |
| `image/tiff` | `public.tiff` | `image/tiff` | `CF_TIFF` |
| `application/pdf` | `com.adobe.pdf` | `application/pdf` | `Portable Document Format` (registered); reported `private`, since Windows has no PDF format applications share |
| `text/rtf` | `public.rtf` | `text/rtf`, `application/rtf` | `Rich Text Format` (registered) |
| `text/html` | `public.html` | `text/html` | `HTML Format` (registered; the CF_HTML header is added on set and removed on get) |
| `text/plain` | `public.utf8-plain-text` | `text/plain;charset=utf-8`, `UTF8_STRING`, `text/plain` | `CF_UNICODETEXT` (UTF-16; UTF-8 on the wire) |

Files cross as their content: the guest stages received files and puts their paths on its
clipboard, so a paste copies real files, and a get reads the files the clipboard names.

**No silent drops.** Stat's `support` lists every canonical format once, in the order
above, with whether the guest holds it, its native name, or why not. A set leaves out what
the guest cannot hold and lists it in `unwritten`; a set of nothing the guest can hold is
answered `unsupported` and leaves the clipboard as it was. A format outside the list is
never written and always listed in `unwritten`.

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
`max_bytes`; that a set of every canonical format reads back byte for byte; files (staged,
named by base name, size-capped and reported when omitted, a bad name refused without
touching the clipboard); content large enough to stream both ways; and the error answers,
`unsupported` included.

`make test-linux-clipboard` runs the Linux module's tests in Docker, as an ordinary user,
against Xvfb, xclip, wl-clipboard and a headless Sway.
