package weaveclipboard

// UseStaging points the default staging at a cache directory and a /var/tmp of
// the test's own, each directory on a filesystem of the f_type fs gives it (0:
// not reported), until the returned function restores them.
func UseStaging(cache func() (string, error), varTmpDir string, fs func(dir string) int64) func() {
	oldCache, oldFS, oldVar := userCacheDir, fsType, varTmp
	userCacheDir, varTmp = cache, varTmpDir
	fsType = func(dir string) (int64, bool) {
		if fs == nil {
			return 0, false
		}
		t := fs(dir)
		return t, t != 0
	}
	return func() { userCacheDir, fsType, varTmp = oldCache, oldFS, oldVar }
}

// Everywhere is every directory on a filesystem of f_type t.
func Everywhere(t int64) func(string) int64 { return func(string) int64 { return t } }
