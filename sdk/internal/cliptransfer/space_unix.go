//go:build unix

package cliptransfer

import "golang.org/x/sys/unix"

// freeBytes is the space an unprivileged user may still write on the
// filesystem holding dir.
func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err //nolint:wrapcheck // described by the caller
	}
	// Bsize is a uint32 on macOS and an int64 on Linux; a disk's size fits
	// an int64 either way.
	avail := int64(st.Bavail) //nolint:gosec // G115: see above
	bsize := int64(st.Bsize)  //nolint:gosec,unconvert // G115: see above
	return avail * bsize, nil
}
