package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	for _, e := range readTarGz(t, members[1].data) {
		control[e.hdr.Name] = e.data
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
