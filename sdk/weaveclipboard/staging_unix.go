//go:build unix

package weaveclipboard

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// makePrivate makes sure dir, which fi describes, is owned by this user and
// closed to everyone else (0700). One another user made first — in the shared
// /var/tmp, say — is refused rather than trusted.
func makePrivate(dir string, fi fs.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s belongs to uid %d", errNotPrivate, dir, st.Uid)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("%w: %w", errNotPrivate, err)
	}
	return nil
}
