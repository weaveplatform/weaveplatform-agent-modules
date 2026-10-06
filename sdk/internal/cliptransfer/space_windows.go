//go:build windows

package cliptransfer

import "golang.org/x/sys/windows"

// freeBytes is the space the calling user may still write on the volume
// holding dir.
func freeBytes(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err //nolint:wrapcheck // described by the caller
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err //nolint:wrapcheck // described by the caller
	}
	return int64(avail), nil //nolint:gosec // G115: a disk's size fits
}
