// Command moduledeb packages one built weave module as a Debian package, for
// local bring-up of a Linux guest from an apt repository.
//
// A capability module is a binary and its module.manifest.json, nothing more.
// This turns that pair into the package core's Linux layout expects: the
// binary at /usr/lib/weave/modules/<id>/<id> (core looks for exactly that
// name) and the manifest beside it, both root-owned, in a package named for
// the module id that depends on weave-agent. Its postinst and postrm tell a
// running weave-agent to reload, so installing, upgrading or removing the
// package takes effect at once. It is stdlib only, so it runs on any host that
// builds Go, including macOS, where there is no dpkg-deb.
//
//	moduledeb -binary weave-linux-presence -manifest module.manifest.json -arch arm64 -out dist
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // dpkg's md5sums file format; not a security claim
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// maintainer matches the maintainer of agent-core's weave-agent package, so
// every package in a bring-up repository names the same one.
const maintainer = "Deployment Theory <support@deploymenttheory.com>"

// modulesDir is where core's Linux layout looks for modules.
const modulesDir = "usr/lib/weave/modules"

var (
	idRe      = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	// debArch maps a Go GOARCH to its Debian architecture name.
	debArch = map[string]string{"amd64": "amd64", "arm64": "arm64"}
)

// The ways a package is refused. Each is wrapped with the detail that names
// the offending file or value.
var (
	errManifest    = errors.New("invalid module manifest")
	errArch        = errors.New("unsupported architecture")
	errNotDeclared = errors.New("platform not declared by the module")
)

// The maintainer scripts every module package carries. Each reloads
// weave-agent when systemd runs and the unit is active, and never fails the
// package operation; weave-agent's watch on its modules directory covers the
// rest (a chroot or an image build). shellcheck reads them as they are here.
var (
	//go:embed scripts/postinst
	postinst []byte
	//go:embed scripts/postrm
	postrm []byte
)

// now is a seam so tests can pin the timestamps written into the archives.
var now = time.Now

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("moduledeb", flag.ContinueOnError)
	fs.SetOutput(stderr)
	binary := fs.String("binary", "", "the built module binary (required)")
	manifest := fs.String("manifest", "", "the module's module.manifest.json (required)")
	arch := fs.String("arch", "",
		"Go architecture the binary was built for: amd64 or arm64 (required)")
	out := fs.String("out", ".", "directory to write the .deb into")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *binary == "" || *manifest == "" || *arch == "" {
		fmt.Fprintln(stderr, "moduledeb: -binary, -manifest and -arch are required")
		return 2
	}
	path, err := build(*binary, *manifest, *arch, *out)
	if err != nil {
		fmt.Fprintln(stderr, "moduledeb:", err)
		return 1
	}
	fmt.Fprintln(stdout, path)
	return 0
}

// moduleManifest is the part of module.manifest.json a package needs.
type moduleManifest struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Platforms []struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
	} `json:"platforms"`
}

func readManifest(path, arch string) (moduleManifest, []byte, error) {
	var m moduleManifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, nil, fmt.Errorf("read manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, nil, fmt.Errorf("%w: %s: %w", errManifest, path, err)
	}
	if !idRe.MatchString(m.ID) {
		return m, nil, fmt.Errorf("%w: %s: invalid module id %q", errManifest, path, m.ID)
	}
	if !versionRe.MatchString(m.Version) {
		return m, nil, fmt.Errorf("%w: %s: invalid version %q", errManifest, path, m.Version)
	}
	// A package for a platform the module does not declare would install a
	// module that core then refuses to run; refuse it here instead.
	for _, p := range m.Platforms {
		if p.OS == "linux" && p.Arch == arch {
			return m, raw, nil
		}
	}
	return m, nil, fmt.Errorf(
		"%w: %s: %s does not declare linux/%s",
		errNotDeclared,
		path,
		m.ID,
		arch,
	)
}

// build writes <id>_<version>_<arch>.deb into outDir and returns its path.
func build(binaryPath, manifestPath, arch, outDir string) (string, error) {
	darch, ok := debArch[arch]
	if !ok {
		return "", fmt.Errorf("%w %q (want amd64 or arm64)", errArch, arch)
	}
	m, manifest, err := readManifest(manifestPath, arch)
	if err != nil {
		return "", err
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return "", fmt.Errorf("read binary: %w", err)
	}

	mtime := now().UTC().Truncate(time.Second)
	dir := modulesDir + "/" + m.ID
	files := []file{
		{name: dir + "/" + m.ID, mode: 0o755, data: binary},
		{name: dir + "/module.manifest.json", mode: 0o644, data: manifest},
	}

	data, dataErr := dataTar(files, mtime)
	control, controlErr := controlTar(m, darch, files, mtime)
	if err := errors.Join(dataErr, controlErr); err != nil {
		return "", err
	}

	var deb bytes.Buffer
	deb.WriteString("!<arch>\n")
	for _, member := range []file{
		{name: "debian-binary", data: []byte("2.0\n")},
		{name: "control.tar.gz", data: control},
		{name: "data.tar.gz", data: data},
	} {
		// ar header: 16 name, 12 mtime, 6 uid, 6 gid, 8 mode, 10 size, 2 magic.
		fmt.Fprintf(
			&deb,
			"%-16s%-12d%-6d%-6d%-8s%-10d`\n",
			member.name,
			mtime.Unix(),
			0,
			0,
			"100644",
			len(member.data),
		)
		deb.Write(member.data)
		if len(member.data)%2 == 1 {
			deb.WriteByte('\n')
		}
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	path := filepath.Join(outDir, fmt.Sprintf("%s_%s_%s.deb", m.ID, m.Version, darch))
	// World-readable: a package is served to apt from a plain repository.
	if err := os.WriteFile(path, deb.Bytes(), 0o644); err != nil { //nolint:gosec // see above
		return "", fmt.Errorf("write package: %w", err)
	}
	return path, nil
}

type file struct {
	name string
	mode int64
	data []byte
}

// controlText is the package's control file.
func controlText(m moduleManifest, darch string, files []file) string {
	var size int
	for _, f := range files {
		size += len(f.data)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Package: %s\n", m.ID)
	fmt.Fprintf(&b, "Version: %s\n", m.Version)
	fmt.Fprintf(&b, "Architecture: %s\n", darch)
	fmt.Fprintf(&b, "Maintainer: %s\n", maintainer)
	// Installed-Size is in KiB, rounded up.
	fmt.Fprintf(&b, "Installed-Size: %d\n", (size+1023)/1024)
	b.WriteString("Depends: weave-agent\n")
	b.WriteString("Section: admin\n")
	b.WriteString("Priority: optional\n")
	fmt.Fprintf(&b, "Description: Weave platform module %s\n", m.ID)
	fmt.Fprintf(&b, " The %s module, run by weave-agent from /%s/%s.\n", m.ID, modulesDir, m.ID)
	return b.String()
}

func controlTar(m moduleManifest, darch string, files []file, mtime time.Time) ([]byte, error) {
	var sums strings.Builder
	for _, f := range files {
		sum := md5.Sum(f.data) //nolint:gosec // see import
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), f.name)
	}
	return tarGz(nil, []file{
		{name: "control", mode: 0o644, data: []byte(controlText(m, darch, files))},
		{name: "md5sums", mode: 0o644, data: []byte(sums.String())},
		{name: "postinst", mode: 0o755, data: postinst},
		{name: "postrm", mode: 0o755, data: postrm},
	}, mtime)
}

// dataTar holds the files and every directory above them, so dpkg creates the
// directories root-owned 0755 rather than inheriting whatever it finds.
func dataTar(files []file, mtime time.Time) ([]byte, error) {
	var dirs []string
	seen := map[string]bool{}
	for _, f := range files {
		parts := strings.Split(filepath.ToSlash(filepath.Dir(f.name)), "/")
		for i := range parts {
			d := strings.Join(parts[:i+1], "/")
			if !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
		}
	}
	return tarGz(dirs, files, mtime)
}

// tarGz writes an in-memory gzipped tar. Every write lands in a buffer, so
// the first error (if any) is kept and returned once at the end.
func tarGz(dirs []string, files []file, mtime time.Time) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	var errs []error
	header := func(name string, typ byte, mode, size int64) {
		errs = append(errs, tw.WriteHeader(&tar.Header{
			Name: "./" + name, Typeflag: typ, Mode: mode, Size: size, ModTime: mtime,
			Uname: "root", Gname: "root", Format: tar.FormatGNU,
		}))
	}
	if len(dirs) > 0 {
		header("", tar.TypeDir, 0o755, 0)
	}
	for _, d := range dirs {
		header(d+"/", tar.TypeDir, 0o755, 0)
	}
	for _, f := range files {
		header(f.name, tar.TypeReg, f.mode, int64(len(f.data)))
		_, err := tw.Write(f.data)
		errs = append(errs, err)
	}
	errs = append(errs, tw.Close(), zw.Close())
	return buf.Bytes(), errors.Join(errs...)
}
