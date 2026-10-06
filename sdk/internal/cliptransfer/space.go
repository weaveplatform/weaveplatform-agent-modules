package cliptransfer

import (
	"errors"
	"fmt"
)

// Reserve is the free space a receiver keeps on its disk beyond what an item
// needs: an item that would leave less is refused, and one whose writing
// would eat into it is stopped. The user's disk filling up because of a
// paste is a worse failure than the paste not arriving.
const Reserve = 256 << 20

// ErrNoSpace reports a disk without room for an item and the reserve.
var ErrNoSpace = errors.New("not enough free disk space")

// FreeBytes reports the free space on the filesystem holding dir. A seam, so
// the tests can run out of space without filling a disk.
var FreeBytes = freeBytes

// CheckSpace reports ErrNoSpace, with the free space, when the disk holding
// dir has less than need bytes plus reserve free. A disk whose free space
// cannot be read is not refused: the writes themselves still fail if it is
// full.
func CheckSpace(dir string, need, reserve int64) (free int64, err error) {
	free, ferr := FreeBytes(dir)
	if ferr != nil {
		return -1, nil //nolint:nilerr // unknown free space is not a refusal
	}
	if free < need+reserve {
		return free, fmt.Errorf("%w: %d bytes needed with a %d-byte reserve, %d free",
			ErrNoSpace, need, reserve, free)
	}
	return free, nil
}
