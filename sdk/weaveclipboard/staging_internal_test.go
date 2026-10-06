package weaveclipboard

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestInMemoryFilesystems(t *testing.T) {
	for typ, want := range map[int64]bool{tmpfsMagic: true, ramfsMagic: true, 0xef53: false} {
		restore := UseStaging(os.UserCacheDir, varTmp, Everywhere(typ))
		if got := inMemory(t.TempDir()); got != want {
			t.Errorf("f_type %#x: in memory %v, want %v", typ, got, want)
		}
		restore()
	}
	restore := UseStaging(os.UserCacheDir, varTmp, nil)
	defer restore()
	if inMemory(t.TempDir()) {
		t.Error("a filesystem that reports no type is taken for memory")
	}
}

// The real statfs reports something for a directory that exists on Linux, and
// nothing for one that does not.
func TestFilesystemType(t *testing.T) {
	_, ok := filesystemType(t.TempDir())
	if ok != (runtime.GOOS == "linux") {
		t.Errorf("reported %v on %s", ok, runtime.GOOS)
	}
	if _, ok := filesystemType(filepath.Join(t.TempDir(), "absent")); ok {
		t.Error("a type for a missing directory")
	}
}

// Staging goes to the user's cache directory; to /var/tmp when that is on tmpfs
// or there is none; and to the temporary directory as the last resort.
func TestStagingFallsBackInOrder(t *testing.T) {
	cache, vt := t.TempDir(), t.TempDir()
	inCache := filepath.Join(cache, "weave", "clipboard")
	inVarTmp := filepath.Join(vt, "weave-clipboard-"+strconv.Itoa(os.Getuid()))
	noCache := func() (string, error) { return "", errors.New("no home") }
	onDisk := func() (string, error) { return cache, nil }
	cacheOnTmpfs := func(dir string) int64 {
		if strings.HasPrefix(dir, cache) {
			return tmpfsMagic
		}
		return 0xef53
	}
	cases := []struct {
		name  string
		cache func() (string, error)
		fs    func(string) int64
		want  string
	}{
		{"the cache directory on disk", onDisk, Everywhere(0xef53), inCache},
		{"the cache directory on tmpfs", onDisk, cacheOnTmpfs, inVarTmp},
		{"no cache directory", noCache, nil, inVarTmp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tc.want == inVarTmp {
				t.Skip("Windows has no /var/tmp: its cache directory is always on disk")
			}
			defer UseStaging(tc.cache, vt, tc.fs)()
			got, err := stagingBase()
			if err != nil || got != tc.want {
				t.Fatalf("staging base %q, %v; want %q", got, err, tc.want)
			}
			if runtime.GOOS != "windows" {
				if fi, err := os.Stat(got); err != nil || fi.Mode().Perm() != 0o700 {
					t.Errorf("mode %v, %v; want 0700", fi.Mode().Perm(), err)
				}
			}
		})
	}

	// Every candidate on tmpfs: the temporary directory all the same.
	tmp := t.TempDir()
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, tmp)
	}
	defer UseStaging(onDisk, vt, Everywhere(tmpfsMagic))()
	got, err := stagingBase()
	if err != nil || filepath.Dir(got) != tmp {
		t.Errorf("last resort %q, %v; want one in %s", got, err, tmp)
	}
}

// A candidate that is not a private directory of this user is passed over.
func TestStagingRefusesWhatIsNotPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no shared /var/tmp to plant a link in")
	}
	cache, vt := t.TempDir(), t.TempDir()
	// The cache candidate is a link to somewhere else.
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cache, "weave"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cache, "weave", "clipboard")); err != nil {
		t.Fatal(err)
	}
	defer UseStaging(func() (string, error) { return cache, nil }, vt, nil)()
	got, err := stagingBase()
	if err != nil || filepath.Dir(got) != vt {
		t.Errorf("staging base %q, %v; want the /var/tmp fallback", got, err)
	}
	// An existing directory is tightened to 0700.
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := makePrivateDir(loose); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(loose); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if err := makePrivateDir(filepath.Join(t.TempDir(), "x", "\x00")); err == nil {
		t.Error("made an impossible directory")
	}
}

// What earlier runs left is cleaned when the module starts; a run in flight is
// not touched.
func TestStartCleansStaleStaging(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	write := func(p string, age time.Duration) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	deadPart := filepath.Join(base, "run-a", "t1", "0", "big.iso.part")
	livePart := filepath.Join(base, "run-b", "t1", "0", "big.iso.part")
	published := filepath.Join(base, "run-a", "t0", "0", "notes.txt")
	write(deadPart, 2*time.Hour)
	write(livePart, time.Minute)
	write(published, 2*time.Hour)
	oldRun := filepath.Join(base, "run-old")
	write(filepath.Join(oldRun, "t0", "0", "f"), staleRun+time.Hour)
	if err := os.Chtimes(
		oldRun,
		now.Add(-staleRun-time.Hour),
		now.Add(-staleRun-time.Hour),
	); err != nil {
		t.Fatal(err)
	}

	s := NewService(nil, WithStagingDir(base))
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{deadPart: false, livePart: true, published: true, oldRun: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", p, err == nil, want)
		}
	}
	cleanStale(filepath.Join(base, "absent"), now) // nothing to clean

	// With no staging directory given, Start cleans the default base.
	cache := t.TempDir()
	defer UseStaging(func() (string, error) { return cache, nil }, t.TempDir(), nil)()
	stale := filepath.Join(cache, "weave", "clipboard", "run-x", "f.part")
	write(stale, 2*time.Hour)
	if err := NewService(nil).Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the default base kept a dead partial: %v", err)
	}
	// Start creates nothing where nothing was staged.
	empty := t.TempDir()
	defer UseStaging(func() (string, error) { return empty, nil }, t.TempDir(), nil)()
	if err := NewService(nil).Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(empty); len(left) != 0 {
		t.Errorf("start made %v", left)
	}
	// And with nowhere to stage, starts all the same.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer UseStaging(func() (string, error) { return blocked, nil }, blocked, nil)()
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, blocked)
	}
	if err := NewService(nil).Start(t.Context()); err != nil {
		t.Error(err)
	}
}
