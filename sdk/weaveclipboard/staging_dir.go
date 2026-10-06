package weaveclipboard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Where a guest stages the files a host sends. They can be any size, so staging
// must be on disk: a temporary directory on tmpfs (/tmp on Fedora and Arch, say)
// would hold a large file in memory, or refuse it as no-space for want of RAM
// when the disk has room.
//
// The default is a directory of the console user's own — the module runs as that
// user — under their cache directory (~/.cache/weave/clipboard on Linux,
// ~/Library/Caches/weave/clipboard on macOS, %LocalAppData%\weave\clipboard on
// Windows). Where that is on tmpfs or ramfs, or there is none, it is
// /var/tmp/weave-clipboard-<uid>, which survives a reboot on disk. Each run of the
// module stages under a directory of its own inside it.

var (
	// Seams, so the tests can choose the user's cache directory, the
	// filesystem a directory is on, and the disk-backed fallback.
	userCacheDir = os.UserCacheDir
	fsType       = filesystemType
	varTmp       = "/var/tmp"
)

// The statfs f_type of the in-memory filesystems (linux/magic.h).
const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

// Stale staging, removed when the module starts: a partial file nothing has
// written to in a while is a transfer that died with an earlier run (one in
// flight writes to its partial file continuously), and a run's directory left
// this long belongs to a session long over.
const (
	stalePartial = time.Hour
	staleRun     = 7 * 24 * time.Hour
)

var errNotPrivate = errors.New("not a private directory of this user")

// inMemory reports whether dir is on tmpfs or ramfs.
func inMemory(dir string) bool {
	t, ok := fsType(dir)
	return ok && (t == tmpfsMagic || t == ramfsMagic)
}

// stagingCandidates are the directories staging may go in, best first.
func stagingCandidates() []string {
	var out []string
	if c, err := userCacheDir(); err == nil && c != "" {
		out = append(out, filepath.Join(c, "weave", "clipboard"))
	}
	if runtime.GOOS != "windows" {
		out = append(out, filepath.Join(varTmp, "weave-clipboard-"+strconv.Itoa(os.Getuid())))
	}
	return out
}

// stagingBase is the first candidate that is a private directory of this user on
// a disk-backed filesystem, made if need be. With none, it is a private directory
// in the system's temporary directory, whatever it is on: staging somewhere beats
// staging nowhere, and the disk guard still refuses what will not fit.
func stagingBase() (string, error) {
	for _, dir := range stagingCandidates() {
		if makePrivateDir(dir) != nil || inMemory(dir) {
			continue
		}
		return dir, nil
	}
	dir := fallbackStaging()
	if err := makePrivateDir(dir); err != nil {
		return "", fmt.Errorf("creating the staging directory: %w", err)
	}
	return dir, nil
}

// fallbackStaging is the last resort: a directory of this user's in the system's
// temporary directory.
func fallbackStaging() string {
	dir := filepath.Join(os.TempDir(), "weave-clipboard")
	if runtime.GOOS != "windows" {
		dir += "-" + strconv.Itoa(os.Getuid())
	}
	return dir
}

// makePrivateDir makes dir if it is missing, and makes sure it is this user's
// alone: a directory, not a link, owned by this user, and closed to everyone else.
func makePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err //nolint:wrapcheck // described by the caller
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err //nolint:wrapcheck // described by the caller
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s", errNotPrivate, dir)
	}
	return makePrivate(dir, fi)
}

// cleanStale removes what earlier runs left in base: partial files no transfer is
// writing any more, and run directories left for a week. A run in flight, of this
// module or of another session of the same user, is left alone.
func cleanStale(base string, now time.Time) {
	// Rooted at base, so a link planted in it cannot steer a removal outside.
	root, err := os.OpenRoot(base)
	if err != nil {
		return
	}
	defer root.Close() //nolint:errcheck // nothing written through it
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.IsDir() &&
			strings.HasPrefix(e.Name(), runPrefix) &&
			now.Sub(info.ModTime()) > staleRun {
			_ = root.RemoveAll(e.Name())
			continue
		}
		_ = fs.WalkDir(root.FS(), e.Name(), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".part") {
				return nil //nolint:nilerr // what cannot be read is left alone
			}
			if info, err := d.Info(); err == nil && now.Sub(info.ModTime()) > stalePartial {
				_ = root.Remove(path)
			}
			return nil
		})
	}
}

// runPrefix names a run's own directory in the staging base.
const runPrefix = "run-"
