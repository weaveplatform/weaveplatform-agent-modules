package weaveclipboardtest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// mem is a clipboard in memory that keeps the contract: the reference the
// contract is checked against. Each knob breaks one promise, so the checks
// can be shown to catch it.
type mem struct {
	mu    sync.Mutex
	token uint64
	items []weavewire.ClipboardItem

	support     *weaveclipboard.Support
	unavailable bool
	single      bool // hold the richest representation only

	frozen      bool                      // the token never moves
	statOff     uint64                    // stat reports the token plus this
	getOff      uint64                    // get reports the token plus this
	readBumps   bool                      // reading moves the token
	statDrops   weavewire.ClipboardFormat // stat leaves this format out
	statFiles   *weavewire.ClipboardFormatInfo
	corrupt     weavewire.ClipboardFormat // read returns other bytes for this
	extraItem   bool                      // read returns a format it was not asked for
	writeOnly   int                       // write keeps only the first n items (0: all)
	writeExtra  bool                      // written lists a format it did not write
	acceptNames bool                      // stage any file name, ".." included
	keepNames   bool                      // read files back under the names sent
	reorder     bool                      // read files back in reverse
	dropFiles   bool                      // read back no files
	skipFiles   bool                      // write leaves the files out
	writeErr    error
	emptyOnFail bool  // a refused write still empties the clipboard
	statSize    int64 // stat sizes every other format at this
}

var errNoClipboard = errors.New("no clipboard here")

func (m *mem) Support() weaveclipboard.Support {
	if m.support != nil {
		return *m.support
	}
	s := weaveclipboard.CanonicalSupport(map[weavewire.ClipboardFormat]string{
		weavewire.ClipboardText: "t", weavewire.ClipboardHTML: "h", weavewire.ClipboardRTF: "r",
		weavewire.ClipboardPNG: "p", weavewire.ClipboardTIFF: "i", weavewire.ClipboardPDF: "d",
		weavewire.ClipboardFiles: "f",
	})
	if m.single {
		s.SingleRepresentation, s.Limitation = true, "one at a time"
	}
	return s
}

func (m *mem) unavailableErr(kind string) error {
	if m.unavailable {
		return &weavewire.UnsupportedError{Kind: kind, Reason: "test"}
	}
	return nil
}

func (m *mem) Stat(context.Context) (weavewire.ClipboardStatResponse, error) {
	if err := m.unavailableErr(weavewire.KindClipboardStat); err != nil {
		return weavewire.ClipboardStatResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := weavewire.ClipboardStatResponse{ChangeToken: m.token + m.statOff}
	files := weavewire.ClipboardFormatInfo{Format: weavewire.ClipboardFiles}
	for _, it := range m.items {
		switch {
		case it.Format == m.statDrops:
		case it.Format == weavewire.ClipboardFiles:
			files.Count++
			files.Size += int64(len(it.Data))
		default:
			st.Formats = append(st.Formats,
				weavewire.ClipboardFormatInfo{Format: it.Format, Size: m.statSize})
		}
	}
	if m.statFiles != nil {
		files = *m.statFiles
	}
	if files.Count > 0 {
		st.Formats = append(st.Formats, files)
	}
	return st, nil
}

func (m *mem) Read(
	_ context.Context,
	formats []weavewire.ClipboardFormat,
	_ int64,
) (weaveclipboard.Contents, error) {
	if err := m.unavailableErr(weavewire.KindClipboardGet); err != nil {
		return weaveclipboard.Contents{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readBumps {
		m.token++
	}
	c := weaveclipboard.Contents{ChangeToken: m.token + m.getOff}
	var files []weavewire.ClipboardItem
	for _, it := range m.items {
		if len(formats) > 0 && !slices.Contains(formats, it.Format) {
			continue
		}
		if it.Format == m.corrupt {
			it.Data = append([]byte("corrupt"), it.Data...)
		}
		if it.Format == weavewire.ClipboardFiles {
			if !m.keepNames {
				it.Name = it.Name[strings.LastIndexAny(it.Name, `/\`)+1:]
			}
			files = append(files, it)
			continue
		}
		c.Items = append(c.Items, it)
	}
	if m.reorder {
		slices.Reverse(files)
	}
	if !m.dropFiles {
		c.Items = append(c.Items, files...)
	}
	if m.extraItem {
		// The service filters for the host; it never reaches the reply.
		c.Items = append(c.Items, weavewire.ClipboardItem{Format: "x/extra", Data: []byte("x")})
	}
	return c, nil
}

func (m *mem) Write(
	_ context.Context,
	items []weavewire.ClipboardItem,
) (weavewire.ClipboardSetResponse, error) {
	if err := m.unavailableErr(weavewire.KindClipboardSet); err != nil {
		return weavewire.ClipboardSetResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fail := func(err error) (weavewire.ClipboardSetResponse, error) {
		if m.emptyOnFail {
			m.items = nil
			m.token++
		}
		return weavewire.ClipboardSetResponse{}, err
	}
	if m.writeErr != nil {
		return fail(m.writeErr)
	}
	for _, it := range items {
		if it.Format != weavewire.ClipboardFiles || m.acceptNames {
			continue
		}
		base := it.Name[strings.LastIndexAny(it.Name, `/\`)+1:]
		if base == "" || base == "." || base == ".." {
			return fail(fmt.Errorf("not a file name: %q", it.Name))
		}
	}
	if m.single {
		for _, f := range weavewire.ClipboardFormats() {
			if i := slices.IndexFunc(
				items,
				func(it weavewire.ClipboardItem) bool { return it.Format == f },
			); i >= 0 {
				items = slices.DeleteFunc(
					slices.Clone(items),
					func(it weavewire.ClipboardItem) bool {
						return it.Format != f
					},
				)
				break
			}
		}
	}
	if m.skipFiles {
		items = slices.DeleteFunc(slices.Clone(items), func(it weavewire.ClipboardItem) bool {
			return it.Format == weavewire.ClipboardFiles
		})
	}
	if m.writeOnly > 0 && len(items) > m.writeOnly {
		items = items[:m.writeOnly]
	}
	if !m.frozen {
		m.token++
	}
	m.items = items
	res := weavewire.ClipboardSetResponse{ChangeToken: m.token}
	for _, it := range items {
		if !slices.Contains(res.Written, it.Format) {
			res.Written = append(res.Written, it.Format)
		}
	}
	if m.writeExtra {
		res.Written = append(res.Written, "x/never")
	}
	return res, nil
}

// The reference backend passes, holding everything at once or one
// representation per set, and answers unsupported when unavailable.
func TestTheReferencePasses(t *testing.T) {
	RunContract(t, Contract{
		New:         func(*testing.T) weaveclipboard.Backend { return &mem{} },
		Unavailable: func(*testing.T) weaveclipboard.Backend { return &mem{unavailable: true} },
	})
	t.Run("single", func(t *testing.T) {
		RunContract(
			t,
			Contract{New: func(*testing.T) weaveclipboard.Backend { return &mem{single: true} }},
		)
	})
	t.Run("without files or PNG", func(t *testing.T) {
		s := (&mem{}).Support()
		for i, f := range s.Formats {
			if f.Format == weavewire.ClipboardFiles || f.Format == weavewire.ClipboardPNG {
				s.Formats[i] = weavewire.ClipboardFormatSupport{
					Format: f.Format,
					Reason: "not here",
				}
			}
		}
		RunContract(
			t,
			Contract{New: func(*testing.T) weaveclipboard.Backend { return &mem{support: &s} }},
		)
	})
	t.Run("files only", func(t *testing.T) {
		s := (&mem{}).Support()
		for i, f := range s.Formats {
			if f.Format != weavewire.ClipboardFiles {
				s.Formats[i] = weavewire.ClipboardFormatSupport{
					Format: f.Format,
					Reason: "not here",
				}
			}
		}
		RunContract(
			t,
			Contract{New: func(*testing.T) weaveclipboard.Backend { return &mem{support: &s} }},
		)
	})
	t.Run("text only", func(t *testing.T) {
		s := (&mem{}).Support()
		for i, f := range s.Formats {
			if f.Format != weavewire.ClipboardText {
				s.Formats[i] = weavewire.ClipboardFormatSupport{
					Format: f.Format,
					Reason: "not here",
				}
			}
		}
		RunContract(
			t,
			Contract{New: func(*testing.T) weaveclipboard.Backend { return &mem{support: &s} }},
		)
	})
}

// recorder stands in for the test a check reports to, so a check run against
// a broken backend can be seen to fail. A fatal report or a skip ends the
// check's goroutine, as it ends a test.
type recorder struct {
	testing.TB
	mu      sync.Mutex
	reports []string
	skipped bool
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, fmt.Sprintf(format, args...))
}

func (r *recorder) Error(args ...any) { r.Errorf("%s", fmt.Sprint(args...)) }

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

func (r *recorder) Fatal(args ...any) {
	r.Error(args...)
	runtime.Goexit()
}

func (r *recorder) Skip(args ...any) {
	r.skipped = true
	runtime.Goexit()
}

func (r *recorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.reports, "\n")
}

// record runs one check against b and returns what it reported.
func record(t *testing.T, check func(testing.TB, *harness), b weaveclipboard.Backend) *recorder {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		check(r, start(r, b))
	}()
	<-done
	return r
}

// Each check catches the broken promise it is there for.
func TestTheChecksCatchBrokenBackends(t *testing.T) {
	canonical := (&mem{}).Support()
	with := func(edit func(*weaveclipboard.Support)) *weaveclipboard.Support {
		s := canonical
		s.Formats = slices.Clone(canonical.Formats)
		edit(&s)
		return &s
	}
	files2 := &weavewire.ClipboardFormatInfo{Format: weavewire.ClipboardFiles, Count: 1, Size: 1}

	cases := []struct {
		name  string
		check func(testing.TB, *harness)
		b     *mem
		want  string
	}{
		{"support: too few", checkSupport, &mem{support: with(func(s *weaveclipboard.Support) {
			s.Formats = s.Formats[1:]
		})}, "want every canonical one"},
		{"support: order", checkSupport, &mem{support: with(func(s *weaveclipboard.Support) {
			s.Formats[0], s.Formats[1] = s.Formats[1], s.Formats[0]
		})}, "support[0] is image/png"},
		{"support: no native", checkSupport, &mem{support: with(func(s *weaveclipboard.Support) {
			s.Formats[2].Native = ""
		})}, "no native name"},
		{"support: no reason", checkSupport, &mem{support: with(func(s *weaveclipboard.Support) {
			s.Formats[2].Held = false
		})}, "no reason"},
		{
			"support: private unheld",
			checkSupport,
			&mem{support: with(func(s *weaveclipboard.Support) {
				s.Formats[2] = weavewire.ClipboardFormatSupport{
					Format:  s.Formats[2].Format,
					Private: true,
					Reason:  "r",
				}
			})},
			"private but not held",
		},
		{
			"support: unexplained single",
			checkSupport,
			&mem{support: with(func(s *weaveclipboard.Support) {
				s.SingleRepresentation = true
			})},
			"no limitation",
		},
		{"support: nothing held", checkSupport, &mem{support: with(func(s *weaveclipboard.Support) {
			for i := range s.Formats {
				s.Formats[i] = weavewire.ClipboardFormatSupport{
					Format: s.Formats[i].Format,
					Reason: "r",
				}
			}
		})}, "holds no format"},

		{"token: frozen", checkToken, &mem{frozen: true}, "was seen before"},
		{"token: stat", checkToken, &mem{statOff: 100}, "stat afterwards"},
		{"token: get", checkToken, &mem{getOff: 100}, "get afterwards"},
		{"token: reads move it", checkToken, &mem{readBumps: true}, "get afterwards"},

		{"every: drops one", checkEveryRepresentation, &mem{writeOnly: 3}, "written"},
		{"every: claims more", checkEveryRepresentation, &mem{writeExtra: true}, "written"},
		{
			"every: stat drops",
			checkEveryRepresentation,
			&mem{statDrops: weavewire.ClipboardHTML},
			"stat does not list text/html",
		},
		{
			"every: stat files",
			checkEveryRepresentation,
			&mem{statFiles: files2},
			"want 2 files of 19 bytes",
		},
		{
			"every: corrupt",
			checkEveryRepresentation,
			&mem{corrupt: weavewire.ClipboardRTF},
			"text/rtf read back",
		},
		{"every: files", checkEveryRepresentation, &mem{dropFiles: true}, "read 0 files"},
		{
			"every: stat sizes",
			checkEveryRepresentation,
			&mem{statSize: 1},
			"stat sizes text/plain at 1 bytes",
		},

		{
			"get: corrupt",
			checkGet,
			&mem{corrupt: weavewire.ClipboardText},
			"a get of text/plain returned",
		},
		{"files: stat", checkFiles, &mem{statFiles: files2}, "want 3 files of 2019 bytes"},
		{"files: names", checkFiles, &mem{keepNames: true}, `want notes.txt`},
		{"files: order", checkFiles, &mem{reorder: true}, "file 0 read back"},
		{"files: not written", checkFiles, &mem{skipFiles: true}, "want files"},
		{"files: bad names", checkFiles, &mem{acceptNames: true}, "want a refusal"},
		{
			"files: refusal moves the token",
			checkFiles,
			&mem{emptyOnFail: true},
			"a refused set moved the token",
		},

		{"large: corrupt", checkLarge, &mem{corrupt: weavewire.ClipboardPNG}, "read back streamed"},
		{"large: not written", checkLarge, &mem{writeExtra: true}, "want image/png"},

		{"errors: mixed", checkErrors, &mem{writeExtra: true}, "beside an unknown format"},
		{"errors: write fails", checkErrors, &mem{writeErr: errors.New("refused")}, "set: "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := record(t, c.check, c.b)
			got := r.all()
			if c.want == "" {
				if got == "" {
					t.Fatal("the check reported nothing")
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("reported %q, want %q", got, c.want)
			}
		})
	}
}

// A backend that is not unavailable when the contract says it is fails,
// and one without files skips the files check, saying so.
func TestTheChecksReportWhatTheyCannotRun(t *testing.T) {
	if r := record(
		t,
		checkUnavailable,
		&mem{},
	); !strings.Contains(
		r.all(),
		"want code unsupported",
	) {
		t.Errorf("reported %q", r.all())
	}
	s := (&mem{}).Support()
	for i, f := range s.Formats {
		if f.Format == weavewire.ClipboardFiles {
			s.Formats[i] = weavewire.ClipboardFormatSupport{Format: f.Format, Reason: "r"}
		}
	}
	if r := record(t, checkFiles, &mem{support: &s}); !r.skipped {
		t.Error("the files check ran without files")
	}
}
