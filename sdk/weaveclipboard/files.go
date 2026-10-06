package weaveclipboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// File is one copied file a backend offers by path, sized and not read: the
// service streams it from disk to a host that fetches it, so no file is ever
// held whole in memory.
type File struct {
	// Name is the file's base name.
	Name string
	// Path is where the file is on this machine.
	Path string
	// Size is its length when it was offered.
	Size int64
}

// FilesAt is the copied files at paths, in order, sized and not read. A path
// that is not a regular file (a folder, or one gone since it was copied) is
// left out: there is nothing of it to copy.
func FilesAt(paths []string) []File {
	var out []File
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, File{Name: filepath.Base(p), Path: p, Size: fi.Size()})
	}
	return out
}

// FileWriter is a Backend that puts files already on disk on the clipboard by
// path. The service stages every file a host sends — streamed to disk as it
// arrives, or written from a set's inline data — and hands the backend their
// paths, so the backend never holds a file in memory and the service can
// stream files of any size. A module whose backend is one says so in every
// stat (weavewire.ClipboardStatResponse.Streaming).
type FileWriter interface {
	// WriteFiles is Write for a set whose files are staged: items are every
	// representation of the set — its files among them, carrying their
	// names and sizes and no data — and paths are the staged files, in the
	// order of the files in items. The paths stay valid until the next
	// write replaces them.
	WriteFiles(
		ctx context.Context,
		items []weavewire.ClipboardItem,
		paths []string,
	) (weavewire.ClipboardSetResponse, error)
}

var errBadName = errors.New("not a file name")

// fileName is the base name a staged file takes from the name a host sent. A
// host that sends a path, or one climbing out with "..", gets its last
// element, never a write outside the staging directory; on Windows a name
// that names an alternate data stream, or holds a character no Windows file
// name may, is refused.
func fileName(name string) (string, error) {
	base := filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	switch base {
	case "", ".", "..", "/", `\`:
		return "", fmt.Errorf("%w: %q", errBadName, name)
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(base, `:*?"<>|`) {
		return "", fmt.Errorf("%w: %q", errBadName, name)
	}
	return base, nil
}
