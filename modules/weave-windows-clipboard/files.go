//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

var errBadName = errors.New("not a file name")

// stager holds the files a host sends, so the clipboard can offer them by
// path: a paste in the guest then copies real files.
type stager struct {
	mu   sync.Mutex
	root string
}

// files writes every files item to its own numbered directory and returns
// their paths in order. Each copy replaces the last one's files, which no
// clipboard references any more. Separate directories keep two files of the
// same name — from different host folders — both pasteable under that name.
func (s *stager) files(items []weavewire.ClipboardItem) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == "" {
		dir, err := os.MkdirTemp("", "weave-clipboard-")
		if err != nil {
			return nil, fmt.Errorf("creating the staging directory: %w", err)
		}
		s.root = dir
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("reading the staging directory: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(s.root, e.Name())); err != nil {
			return nil, fmt.Errorf("clearing the staging directory: %w", err)
		}
	}
	var paths []string
	for _, it := range items {
		if it.Format != weavewire.ClipboardFiles {
			continue
		}
		name, err := fileName(it.Name)
		if err != nil {
			return nil, err
		}
		dir := filepath.Join(s.root, strconv.Itoa(len(paths)))
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, fmt.Errorf("staging %s: %w", name, err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, it.Data, 0o600); err != nil {
			return nil, fmt.Errorf("staging %s: %w", name, err)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// fileName is the base name of a name the host sent. A host that sends a
// path, or one climbing out with "..", gets its last element, never a write
// outside the staging directory.
func fileName(name string) (string, error) {
	base := filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	switch base {
	case "", ".", "..", "/", `\`:
		return "", fmt.Errorf("%w: %q", errBadName, name)
	}
	// A colon names an alternate data stream, and the rest no Windows file
	// name may hold.
	if strings.ContainsAny(base, `:*?"<>|`) {
		return "", fmt.Errorf("%w: %q", errBadName, name)
	}
	return base, nil
}

// readFiles reads each copied file's content: the paths mean nothing on the
// host, so the content is what crosses. A directory or unreadable path is
// skipped, and a file over maxBytes is returned with its size and no data so
// the service lists it as omitted rather than truncating it.
func readFiles(paths []string, maxBytes int64) []weavewire.ClipboardItem {
	var out []weavewire.ClipboardItem
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		it := weavewire.ClipboardItem{
			Format: weavewire.ClipboardFiles,
			Name:   filepath.Base(p),
			Size:   fi.Size(),
		}
		if fi.Size() <= maxBytes {
			data, err := os.ReadFile(p) //nolint:gosec // G304: the path is one the user copied
			if err != nil {
				continue
			}
			it.Data, it.Size = data, int64(len(data))
		}
		out = append(out, it)
	}
	return out
}

// dropFiles is a CF_HDROP block: a DROPFILES header — the offset of the path
// list, a drop point and two flags, of which fWide marks the paths UTF-16 —
// then the paths, each NUL-terminated, and a NUL ending the list. It is
// byte for byte what Explorer puts there for a copy.
func dropFiles(paths []string) []byte {
	const headerSize = 20 // pFiles, pt.x, pt.y, fNC, fWide: five 32-bit fields
	out := binary.LittleEndian.AppendUint32(nil, headerSize)
	out = binary.LittleEndian.AppendUint32(out, 0) // pt.x
	out = binary.LittleEndian.AppendUint32(out, 0) // pt.y
	out = binary.LittleEndian.AppendUint32(out, 0) // fNC
	out = binary.LittleEndian.AppendUint32(out, 1) // fWide
	for _, p := range paths {
		out = append(out, encodeUTF16(p)...)
	}
	return append(out, 0, 0)
}
