//go:build linux

package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// parseURIList reads a text/uri-list (RFC 2483): one URI per line, CRLF or
// LF, lines starting with # are comments. Only local file URIs name files the
// guest can read; anything else (an http link a browser put there) is not a
// copied file.
func parseURIList(data []byte) []string {
	var paths []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
			continue
		}
		if u.Path != "" {
			paths = append(paths, u.Path)
		}
	}
	return paths
}

// uriList renders paths as a text/uri-list, CRLF-terminated as RFC 2483
// requires and file managers expect.
func uriList(paths []string) []byte {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(fileURI(p))
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

// fileURI is the file:// URI of a local path.
func fileURI(p string) string { return (&url.URL{Scheme: "file", Path: p}).String() }

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
	case "", ".", "..", "/":
		return "", fmt.Errorf("%w: %q", errBadName, name)
	}
	return base, nil
}
