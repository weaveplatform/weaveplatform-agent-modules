package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const presenceManifest = `{"schema":1,"id":"weave-linux-presence","version":"0.1.0","protocol":1,
"zone":"A","privilege":"service","session":"system",
"platforms":[{"os":"linux","arch":"amd64"},{"os":"linux","arch":"arm64"}]}`

func pinNow(t *testing.T) time.Time {
	t.Helper()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	orig := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = orig })
	return at
}

// fixture writes a module binary and manifest and returns their paths.
func fixture(t *testing.T, manifest string, binary []byte) (bin, man string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "weave-linux-presence")
	man = filepath.Join(dir, "module.manifest.json")
	if err := os.WriteFile(bin, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(man, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin, man
}

type arEntry struct {
	name  string
	mtime int64
	data  []byte
}

// readAr parses the archive the way dpkg does: fixed 60-byte
// headers, members padded to an even offset.
func readAr(t *testing.T, raw []byte) []arEntry {
	t.Helper()
	if !bytes.HasPrefix(raw, []byte("!<arch>\n")) {
		t.Fatal("not an ar archive")
	}
	raw = raw[8:]
	var out []arEntry
	for len(raw) > 0 {
		hdr := raw[:60]
		if string(hdr[58:60]) != "`\n" {
			t.Fatalf("bad ar header magic %q", hdr[58:60])
		}
		size, err := strconv.Atoi(strings.TrimSpace(string(hdr[48:58])))
		if err != nil {
			t.Fatal(err)
		}
		mtime, _ := strconv.ParseInt(strings.TrimSpace(string(hdr[16:28])), 10, 64)
		out = append(out, arEntry{strings.TrimSpace(string(hdr[0:16])), mtime, raw[60 : 60+size]})
		raw = raw[60+size+size%2:]
	}
	return out
}

type tarEntry struct {
	hdr  *tar.Header
	data string
}

func readTarGz(t *testing.T, raw []byte) []tarEntry {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var out []tarEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out = append(out, tarEntry{h, string(b)})
	}
}

func TestBuildPackagesTheModule(t *testing.T) {
	at := pinNow(t)
	binary := bytes.Repeat([]byte{0x7f}, 3000)
	bin, man := fixture(t, presenceManifest, binary)
	out := t.TempDir()

	path, err := build(bin, man, "arm64", out)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "weave-linux-presence_0.1.0_arm64.deb" {
		t.Fatalf("package file %s", filepath.Base(path))
	}
	raw, _ := os.ReadFile(path)
	members := readAr(t, raw)
	if len(members) != 3 || members[0].name != "debian-binary" ||
		members[1].name != "control.tar.gz" ||
		members[2].name != "data.tar.gz" {
		t.Fatalf("members %+v", members)
	}
	if string(members[0].data) != "2.0\n" || members[0].mtime != at.Unix() {
		t.Fatalf("debian-binary %q at %d", members[0].data, members[0].mtime)
	}

	control := map[string]string{}
	var controlOrder []string
	for _, e := range readTarGz(t, members[1].data) {
		control[e.hdr.Name] = e.data
		controlOrder = append(controlOrder, e.hdr.Name)
	}
	if got := strings.Join(controlOrder, " "); got != "./control ./md5sums ./postinst ./postrm" {
		t.Fatalf("control entries %s", got)
	}
	want := []string{
		"Package: weave-linux-presence\n",
		"Version: 0.1.0\n",
		"Architecture: arm64\n",
		"Maintainer: Deployment Theory <support@deploymenttheory.com>\n",
		"Installed-Size: 4\n",
		"Depends: weave-agent\n",
		"Description: Weave platform module weave-linux-presence\n",
	}
	for _, w := range want {
		if !strings.Contains(control["./control"], w) {
			t.Errorf("control lacks %q:\n%s", w, control["./control"])
		}
	}
	if !strings.Contains(
		control["./md5sums"],
		"  usr/lib/weave/modules/weave-linux-presence/weave-linux-presence\n",
	) {
		t.Errorf("md5sums:\n%s", control["./md5sums"])
	}

	got := map[string]tarEntry{}
	var order []string
	for _, e := range readTarGz(t, members[2].data) {
		if e.hdr.Uid != 0 || e.hdr.Gid != 0 || e.hdr.Uname != "root" || e.hdr.Gname != "root" {
			t.Errorf("%s not root-owned: %+v", e.hdr.Name, e.hdr)
		}
		if !e.hdr.ModTime.Equal(at) {
			t.Errorf("%s mtime %v", e.hdr.Name, e.hdr.ModTime)
		}
		got[e.hdr.Name] = e
		order = append(order, e.hdr.Name)
	}
	wantOrder := []string{
		"./", "./usr/", "./usr/lib/", "./usr/lib/weave/", "./usr/lib/weave/modules/",
		"./usr/lib/weave/modules/weave-linux-presence/",
		"./usr/lib/weave/modules/weave-linux-presence/weave-linux-presence",
		"./usr/lib/weave/modules/weave-linux-presence/module.manifest.json",
	}
	if strings.Join(order, " ") != strings.Join(wantOrder, " ") {
		t.Fatalf("data entries %v", order)
	}
	for _, d := range wantOrder[:6] {
		if got[d].hdr.Typeflag != tar.TypeDir || got[d].hdr.Mode != 0o755 {
			t.Errorf("%s: %+v", d, got[d].hdr)
		}
	}
	b := got[wantOrder[6]]
	if b.hdr.Mode != 0o755 || b.data != string(binary) {
		t.Errorf("binary mode %o, %d bytes", b.hdr.Mode, len(b.data))
	}
	m := got[wantOrder[7]]
	if m.hdr.Mode != 0o644 || m.data != presenceManifest {
		t.Errorf("manifest mode %o: %s", m.hdr.Mode, m.data)
	}
}

// Members of every size parity must stay aligned, or dpkg reads garbage.
func TestBuildPadsOddMembers(t *testing.T) {
	pinNow(t)
	for n := 1; n <= 8; n++ {
		bin, man := fixture(t, presenceManifest, bytes.Repeat([]byte{byte(n)}, n*37))
		path, err := build(bin, man, "amd64", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(path)
		if got := readAr(t, raw); len(got) != 3 {
			t.Fatalf("size %d: %d members", n, len(got))
		}
		if !strings.HasSuffix(path, "_amd64.deb") {
			t.Fatalf("path %s", path)
		}
	}
}

func TestBuildRefuses(t *testing.T) {
	bin, man := fixture(t, presenceManifest, []byte("x"))
	write := func(content string) string {
		p := filepath.Join(t.TempDir(), "module.manifest.json")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// An output path that is already a directory where the .deb should go. The
	// OS wording of these two errors differs on Windows, so only failure is checked.
	clash := t.TempDir()
	if err := os.Mkdir(
		filepath.Join(clash, "weave-linux-presence_0.1.0_arm64.deb"),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		bin, man, arch, out, want string
	}{
		"arch":        {bin, man, "riscv64", t.TempDir(), "unsupported architecture"},
		"no manifest": {bin, filepath.Join(t.TempDir(), "nope"), "arm64", t.TempDir(), "nope"},
		"bad json":    {bin, write("{"), "arm64", t.TempDir(), "unexpected end"},
		"bad id": {
			bin,
			write(`{"id":"../x","version":"1.0.0"}`),
			"arm64",
			t.TempDir(),
			"invalid module id",
		},
		"bad version": {
			bin,
			write(`{"id":"m","version":"one"}`),
			"arm64",
			t.TempDir(),
			"invalid version",
		},
		"undeclared": {
			bin,
			write(`{"id":"m","version":"1.0.0","platforms":[{"os":"darwin","arch":"arm64"}]}`),
			"arm64",
			t.TempDir(),
			"does not declare linux/arm64",
		},
		"no binary":     {filepath.Join(t.TempDir(), "nope"), man, "arm64", t.TempDir(), "nope"},
		"out is a file": {bin, man, "arm64", filepath.Join(file, "sub"), ""},
		"write fails":   {bin, man, "arm64", clash, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := build(c.bin, c.man, c.arch, c.out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestRun(t *testing.T) {
	pinNow(t)
	bin, man := fixture(t, presenceManifest, []byte("x"))
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	args := []string{"-binary", bin, "-manifest", man, "-arch", "arm64", "-out", out}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	want := filepath.Join(out, "weave-linux-presence_0.1.0_arm64.deb")
	if strings.TrimSpace(stdout.String()) != want {
		t.Fatalf("stdout %q", stdout.String())
	}

	for name, c := range map[string]struct {
		args []string
		code int
		want string
	}{
		"help":       {[]string{"-h"}, 0, "-binary"},
		"bad flag":   {[]string{"-nope"}, 2, "-nope"},
		"missing":    {[]string{"-binary", bin}, 2, "required"},
		"build fail": {[]string{"-binary", bin, "-manifest", man, "-arch", "386"}, 1, "unsupported architecture"},
	} {
		t.Run(name, func(t *testing.T) {
			var o, e bytes.Buffer
			code := run(c.args, &o, &e)
			if code != c.code || !strings.Contains(e.String(), c.want) {
				t.Fatalf("code %d, stderr %q", code, e.String())
			}
		})
	}
}

// Callers tell the refusals apart by sentinel, not by message.
func TestBuildRefusalSentinels(t *testing.T) {
	bin, man := fixture(t, presenceManifest, []byte("x"))
	if _, err := build(bin, man, "riscv64", t.TempDir()); !errors.Is(err, errArch) {
		t.Errorf("arch: %v", err)
	}
	if _, err := build(bin, man, "amd64", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "module.manifest.json")
	if err := os.WriteFile(other, []byte(`{"id":"m","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := build(bin, other, "amd64", t.TempDir()); !errors.Is(err, errNotDeclared) {
		t.Errorf("undeclared: %v", err)
	}
	if err := os.WriteFile(other, []byte(`{"id":"M","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := build(bin, other, "amd64", t.TempDir()); !errors.Is(err, errManifest) {
		t.Errorf("manifest: %v", err)
	}
}

// The control archive carries the maintainer scripts as dpkg runs them:
// executable, root-owned, byte for byte the scripts shellcheck reads.
func TestControlArchiveCarriesMaintainerScripts(t *testing.T) {
	at := pinNow(t)
	bin, man := fixture(t, presenceManifest, []byte("x"))
	path, err := build(bin, man, "arm64", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	modes := map[string]int64{
		"./control": 0o644, "./md5sums": 0o644, "./postinst": 0o755, "./postrm": 0o755,
	}
	scripts := map[string][]byte{"./postinst": postinst, "./postrm": postrm}
	for _, e := range readTarGz(t, readAr(t, raw)[1].data) {
		want, ok := modes[e.hdr.Name]
		if !ok {
			t.Errorf("unexpected control entry %s", e.hdr.Name)
			continue
		}
		delete(modes, e.hdr.Name)
		if e.hdr.Typeflag != tar.TypeReg || e.hdr.Mode != want {
			t.Errorf(
				"%s: type %c mode %o, want regular %o",
				e.hdr.Name,
				e.hdr.Typeflag,
				e.hdr.Mode,
				want,
			)
		}
		if e.hdr.Uid != 0 || e.hdr.Gid != 0 || e.hdr.Uname != "root" || e.hdr.Gname != "root" ||
			!e.hdr.ModTime.Equal(at) {
			t.Errorf("%s: %+v", e.hdr.Name, e.hdr)
		}
		if s, ok := scripts[e.hdr.Name]; ok && e.data != string(s) {
			t.Errorf("%s differs from scripts/%s", e.hdr.Name, strings.TrimPrefix(e.hdr.Name, "./"))
		}
		// md5sums covers the installed files, never the control members.
		if e.hdr.Name == "./md5sums" && strings.Contains(e.data, "post") {
			t.Errorf("md5sums lists a maintainer script:\n%s", e.data)
		}
	}
	if len(modes) != 0 {
		t.Errorf("missing control entries %v", modes)
	}
}

// Each script is a POSIX sh script that stops on error and reloads only an
// active weave-agent under systemd, never failing on the reload itself.
func TestMaintainerScriptsShape(t *testing.T) {
	for name, s := range map[string][]byte{"postinst": postinst, "postrm": postrm} {
		text := string(s)
		if !strings.HasPrefix(text, "#!/bin/sh\n") {
			t.Errorf("%s: no #!/bin/sh line", name)
		}
		for _, want := range []string{
			"\nset -e\n",
			"[ -d /run/systemd/system ]",
			"systemctl is-active --quiet weave-agent",
			"systemctl reload weave-agent || true",
			`case "$1" in`,
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
}

// runScript runs a maintainer script with a fake systemctl first on PATH. The
// fake logs each call, reports the unit as active when active is set, and
// fails every reload, which the script must swallow.
func runScript(t *testing.T, script []byte, active bool, args ...string) (int, string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("no POSIX sh")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	state := "3"
	if active {
		state = "0"
	}
	fake := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" +
		"case \"$1\" in is-active) exit " + state + ";; *) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "script")
	if err := os.WriteFile(path, script, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, append([]string{path}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, _ := cmd.CombinedOutput()
	calls, _ := os.ReadFile(log)
	if len(out) > 0 && cmd.ProcessState.ExitCode() == 0 {
		t.Errorf("%v printed %q", args, out)
	}
	return cmd.ProcessState.ExitCode(), string(calls)
}

// The scripts handle every action dpkg passes them: they reload on the ones
// that change the installed module, do nothing on the rest, and refuse an
// unknown action as Debian policy asks. Whether a reload is attempted depends
// on systemd running on this host, so that is checked against the host.
func TestMaintainerScriptActions(t *testing.T) {
	_, statErr := os.Stat("/run/systemd/system")
	systemd := statErr == nil
	cases := []struct {
		name   string
		script []byte
		args   []string
		reload bool
	}{
		{"postinst", postinst, []string{"configure", "0.1.0"}, true},
		{"postinst", postinst, []string{"abort-upgrade", "0.1.0"}, false},
		{"postinst", postinst, []string{"abort-remove"}, false},
		{"postinst", postinst, []string{"abort-deconfigure", "in-favour", "x", "1"}, false},
		{"postrm", postrm, []string{"remove"}, true},
		{"postrm", postrm, []string{"purge"}, true},
		{"postrm", postrm, []string{"upgrade", "0.2.0"}, false},
		{"postrm", postrm, []string{"failed-upgrade", "0.1.0"}, false},
		{"postrm", postrm, []string{"abort-install"}, false},
		{"postrm", postrm, []string{"abort-upgrade", "0.1.0"}, false},
		{"postrm", postrm, []string{"disappear", "other", "1"}, false},
	}
	for _, c := range cases {
		for _, active := range []bool{true, false} {
			t.Run(c.name+" "+strings.Join(c.args, " "), func(t *testing.T) {
				code, calls := runScript(t, c.script, active, c.args...)
				if code != 0 {
					t.Fatalf("exit %d", code)
				}
				reloaded := strings.Contains(calls, "reload weave-agent")
				if want := c.reload && active && systemd; reloaded != want {
					t.Fatalf("reloaded %v, want %v (systemd %v, active %v): %q",
						reloaded, want, systemd, active, calls)
				}
				if !c.reload && calls != "" {
					t.Fatalf("systemctl called on %v: %q", c.args, calls)
				}
			})
		}
	}
	for name, s := range map[string][]byte{"postinst": postinst, "postrm": postrm} {
		if code, calls := runScript(t, s, true, "bogus"); code != 1 || calls != "" {
			t.Errorf("%s bogus: exit %d, calls %q", name, code, calls)
		}
	}
}
