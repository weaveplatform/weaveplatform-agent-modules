//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// tool is one command-line clipboard client.
type tool struct {
	// list lists the clipboard's targets, one per line.
	list []string
	// pasteArgs and copyArgs build the read and write of one target.
	pasteArgs func(target string) []string
	copyArgs  func(target string) []string
	// text is the target plain text is written as: the one the tool's
	// server treats as UTF-8 text.
	text string
	cmd  string // the binary run for list and paste
	cpy  string // the binary run for copy
}

func wlClipboard() *tool {
	return &tool{
		cmd:       "wl-paste",
		cpy:       "wl-copy",
		list:      []string{"--list-types"},
		pasteArgs: func(t string) []string { return []string{"--no-newline", "--type", t} },
		copyArgs:  func(t string) []string { return []string{"--type", t} },
		text:      "text/plain;charset=utf-8",
	}
}

func xclip() *tool {
	return &tool{
		cmd:  "xclip",
		cpy:  "xclip",
		list: []string{"-selection", "clipboard", "-t", "TARGETS", "-o"},
		pasteArgs: func(t string) []string {
			return []string{"-selection", "clipboard", "-t", t, "-o"}
		},
		copyArgs: func(t string) []string { return []string{"-selection", "clipboard", "-t", t, "-i"} },
		text:     "UTF8_STRING",
	}
}

func lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// commandTimeout bounds one tool invocation. A clipboard owner that never
// answers a request (a hung application) would otherwise hang the op.
const commandTimeout = 5 * time.Second

// targets lists what the clipboard offers. An empty clipboard is no targets,
// not a failure: both tools exit non-zero when nothing owns the clipboard,
// which is indistinguishable from their other refusals and far more common.
// A tool that cannot be run at all is a failure.
func (t *tool) targets(ctx context.Context) ([]string, error) {
	out, err := t.output(ctx, t.list)
	if err != nil {
		if _, exited := errors.AsType[*exec.ExitError](err); exited {
			return nil, nil
		}
		return nil, err
	}
	return splitLines(out), nil
}

func (t *tool) paste(ctx context.Context, target string) ([]byte, error) {
	return t.output(ctx, t.pasteArgs(target))
}

func (t *tool) output(ctx context.Context, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	// The binaries are fixed; only a target name varies, passed as one
	// argument with no shell between.
	cmd := exec.CommandContext(ctx, t.cmd, args...) //nolint:gosec // G204
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", t.cmd, strings.Join(args, " "), err)
	}
	return out, nil
}

// copy offers data as target.
//
// Both tools fork a process that stays behind to serve the clipboard until
// something else takes it. That process inherits the command's output, so
// they are left unattached: capturing them would make Run wait for a pipe the
// server holds open for as long as the copy lasts.
func (t *tool) copy(ctx context.Context, target string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	args := t.copyArgs(target)
	cmd := exec.CommandContext(ctx, t.cpy, args...) //nolint:gosec // G204: as in output
	cmd.Stdin = bytes.NewReader(data)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", t.cpy, strings.Join(args, " "), err)
	}
	return nil
}

// writeTarget is the target a format is written as.
func (t *tool) writeTarget(f weavewire.ClipboardFormat) string {
	if f == weavewire.ClipboardText {
		return t.text
	}
	return targetsFor[f][0]
}

// targetsFor lists, for each format a Linux clipboard can carry, the targets
// it may be offered under, preferred first. Applications disagree: GTK offers
// text/plain;charset=utf-8, X11 clients UTF8_STRING, and RTF appears under
// two MIME types.
var targetsFor = map[weavewire.ClipboardFormat][]string{
	weavewire.ClipboardText: {"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "STRING"},
	weavewire.ClipboardHTML: {"text/html"},
	weavewire.ClipboardRTF:  {"text/rtf", "application/rtf"},
	weavewire.ClipboardPNG:  {"image/png"},
	weavewire.ClipboardTIFF: {"image/tiff"},
	weavewire.ClipboardPDF:  {"application/pdf"},
	// A list of file:// URIs: what file managers put on the clipboard for a
	// copy, and accept on paste.
	weavewire.ClipboardFiles: {"text/uri-list"},
}

// pickTarget finds the target the clipboard offers format f under. Targets
// are compared case-insensitively and exactly — text/plain;charset=utf-8 is
// not text/plain, which may be in the locale's encoding.
func pickTarget(offered []string, f weavewire.ClipboardFormat) (string, bool) {
	for _, want := range targetsFor[f] {
		for _, o := range offered {
			if strings.EqualFold(normalise(o), normalise(want)) {
				return o, true
			}
		}
	}
	return "", false
}

// normalise drops the whitespace some applications put around a MIME
// parameter ("text/plain; charset=utf-8").
func normalise(t string) string { return strings.ReplaceAll(t, " ", "") }

func splitLines(b []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
