//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// tool is the clipboard through wl-clipboard's wl-paste and wl-copy: the
// fallback for a Wayland session whose compositor has no data-control
// protocol and that has no X display to reach the clipboard through either.
//
// wl-copy offers one target per copy — a second invocation replaces the
// first rather than adding to it — so this mechanism holds one representation
// per set, and stat says so.
type tool struct {
	cmd, cpy string // the binaries run for list and paste, and for copy
	// sets counts this module's own writes. It is part of the token, so a set
	// changes the token even when it writes what the clipboard already held.
	sets atomic.Uint64
}

func wlClipboard() *tool { return &tool{cmd: "wl-paste", cpy: "wl-copy"} }

func (t *tool) name() string { return "wl-copy" }

func (t *tool) single() bool { return true }

func (t *tool) broken() bool { return false }

func (t *tool) close() {}

func lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// commandTimeout bounds one tool invocation. A clipboard owner that never
// answers a request (a hung application) would otherwise hang the op.
const commandTimeout = 5 * time.Second

// offered lists what the clipboard offers, with a digest of its content as
// the token.
//
// The tools see no change counter — the core Wayland protocol numbers no
// selection change a client without focus can watch — so the token is a
// digest of the content itself: the target list and every mapped
// representation's bytes. That reads the clipboard on every stat, which costs
// a process per format and the bytes of an image when one is copied; a
// digest of the targets alone would be cheap, but would miss a second image
// copied over the first, which offers exactly the same targets. A sync loop
// that never sees a change is worse than one that reads a few hundred
// kilobytes a second. The module's own sets are counted into it too, so every
// set moves the token.
func (t *tool) offered(ctx context.Context) ([]string, uint64, error) {
	targets, err := t.targets(ctx)
	if err != nil {
		return nil, 0, err
	}
	h := fnv.New64a()
	_, _ = h.Write(binary.LittleEndian.AppendUint64(nil, t.sets.Load()))
	for _, target := range targets {
		_, _ = h.Write([]byte(target))
		_, _ = h.Write([]byte{0})
	}
	for _, r := range formatsOffered(targets) {
		data, err := t.read(ctx, r.target)
		if err != nil {
			continue // the owner changed between the list and the read
		}
		_, _ = h.Write([]byte(r.format))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
	}
	return targets, h.Sum64(), nil
}

// targets lists what the clipboard offers. An empty clipboard is no targets,
// not a failure: wl-paste exits non-zero when nothing owns the clipboard,
// which is indistinguishable from its other refusals and far more common. A
// tool that cannot be run at all is a failure.
func (t *tool) targets(ctx context.Context) ([]string, error) {
	out, err := t.output(ctx, "--list-types")
	if err != nil {
		if _, exited := errors.AsType[*exec.ExitError](err); exited {
			return nil, nil
		}
		return nil, err
	}
	return splitLines(out), nil
}

func (t *tool) read(ctx context.Context, target string) ([]byte, error) {
	return t.output(ctx, "--no-newline", "--type", target)
}

func (t *tool) output(ctx context.Context, args ...string) ([]byte, error) {
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

// own offers the first offer's data as its target.
//
// wl-copy forks a process that stays behind to serve the clipboard until
// something else takes it. That process inherits the command's output, so it
// is left unattached: capturing it would make Run wait for a pipe the server
// holds open for as long as the copy lasts.
func (t *tool) own(ctx context.Context, offers []offer) (uint64, error) {
	o := offers[0]
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, t.cpy, "--type", o.target) //nolint:gosec // G204: as in output
	cmd.Stdin = bytes.NewReader(o.data)
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("%s --type %s: %w", t.cpy, o.target, err)
	}
	t.sets.Add(1)
	_, tok, err := t.offered(ctx)
	return tok, err
}

func splitLines(b []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
