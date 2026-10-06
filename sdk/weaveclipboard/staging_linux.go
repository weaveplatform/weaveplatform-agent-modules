//go:build linux

package weaveclipboard

import "golang.org/x/sys/unix"

// filesystemType is the statfs f_type of the filesystem holding dir.
func filesystemType(dir string) (int64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Type), true //nolint:unconvert // Type's width differs per architecture
}
