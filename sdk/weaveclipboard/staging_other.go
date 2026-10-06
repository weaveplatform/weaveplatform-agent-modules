//go:build !linux

package weaveclipboard

// filesystemType reports nothing outside Linux: macOS and Windows keep their
// per-user cache directories on disk.
func filesystemType(string) (int64, bool) { return 0, false }
