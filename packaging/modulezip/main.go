// Command modulezip packages one built weave Windows module as a zip that
// installs itself: the counterpart of moduledeb and modulepkg.
//
// A capability module is a binary and its module.manifest.json, nothing more.
// The zip carries the pair under module\ (the binary named <id>.exe, which is
// the name core's discovery looks for) beside two PowerShell scripts:
//
//	install.ps1    copies the module to <%ProgramFiles%\Weave>\modules\<id>\,
//	               installs the uninstaller and asks weave-agent to reload
//	uninstall.ps1  removes it again; installed as uninstall.d\<id>.ps1
//
// A zip and a script, not an MSI, because that is how core itself installs on
// Windows (its release zip and install.ps1), so the media and unattend steps
// that install core install a module the same way, and nothing here needs
// WiX or a Windows build machine. The scripts are fixed files that read the
// module id from the package, so the release signs them once with Authenticode
// and hands the signed copies to -scripts; the binary inside is signed before
// it is packaged.
//
// It is stdlib Go and needs no tool, so it runs on any OS.
//
//	modulezip -binary weave-windows-presence.exe -manifest module.manifest.json -arch amd64 -out dist
package main

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var (
	idRe      = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	// archs are the Go architectures a Windows module may be packaged for.
	archs = map[string]bool{"amd64": true, "arm64": true}
)

// The ways a package is refused. Each is wrapped with the detail that names
// the offending file or value.
var (
	errManifest    = errors.New("invalid module manifest")
	errArch        = errors.New("unsupported architecture")
	errNotDeclared = errors.New("platform not declared by the module")
	errNotPE       = errors.New("not a Windows executable")
)

// The scripts every package carries unless -scripts names signed copies.
var (
	//go:embed scripts/install.ps1
	installScript []byte
	//go:embed scripts/uninstall.ps1
	uninstallScript []byte
)

// scriptNames are the files -scripts must hold, as the zip names them.
var scriptNames = []string{"install.ps1", "uninstall.ps1"}

// modTime is every entry's timestamp: the zip format's epoch, so a package
// is the same bytes whenever it is built from the same files.
var modTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options are what one package build needs.
type options struct {
	binary, manifest, arch, out string
	// scripts, when set, is a directory holding install.ps1 and
	// uninstall.ps1 to package in place of the built-in ones: the release's
	// Authenticode-signed copies.
	scripts string
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("modulezip", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(
		&o.binary,
		"binary",
		"",
		"the built module binary, <id>.exe or any name (required)",
	)
	fs.StringVar(&o.manifest, "manifest", "", "the module's module.manifest.json (required)")
	fs.StringVar(
		&o.arch,
		"arch",
		"amd64",
		"Go architecture the binary was built for: amd64 or arm64",
	)
	fs.StringVar(&o.out, "out", ".", "directory to write the .zip into")
	fs.StringVar(&o.scripts, "scripts", "",
		"directory holding install.ps1 and uninstall.ps1 to package instead of the built-in ones")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if o.binary == "" || o.manifest == "" {
		fmt.Fprintln(stderr, "modulezip: -binary and -manifest are required")
		return 2
	}
	path, err := build(o)
	if err != nil {
		fmt.Fprintln(stderr, "modulezip:", err)
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
		if p.OS == "windows" && p.Arch == arch {
			return m, raw, nil
		}
	}
	return m, nil, fmt.Errorf("%w: %s: %s does not declare windows/%s",
		errNotDeclared, path, m.ID, arch)
}

// entry is one file in the zip, by its slash-separated name.
type entry struct {
	name string
	data []byte
}

// readScripts returns the install and uninstall scripts: the built-in ones,
// or those in dir.
func readScripts(dir string) (install, uninstall []byte, err error) {
	if dir == "" {
		return installScript, uninstallScript, nil
	}
	var got [2][]byte
	for i, name := range scriptNames {
		if got[i], err = os.ReadFile(filepath.Join(dir, name)); err != nil {
			return nil, nil, fmt.Errorf("read scripts: %w", err)
		}
	}
	return got[0], got[1], nil
}

// contents lists the zip's files. The scripts are at the top, where an
// operator unpacking it looks first; the module is under module\ with the
// binary named for core.
func contents(m moduleManifest, binary, manifest, install, uninstall []byte) []entry {
	return []entry{
		{name: "install.ps1", data: install},
		{name: "uninstall.ps1", data: uninstall},
		{name: "module/" + m.ID + ".exe", data: binary},
		{name: "module/module.manifest.json", data: manifest},
	}
}

// build writes <id>_<version>_windows_<arch>.zip into o.out and returns its
// path.
func build(o options) (string, error) {
	if !archs[o.arch] {
		return "", fmt.Errorf("%w %q (want amd64 or arm64)", errArch, o.arch)
	}
	m, manifest, err := readManifest(o.manifest, o.arch)
	if err != nil {
		return "", err
	}
	binary, err := os.ReadFile(o.binary)
	if err != nil {
		return "", fmt.Errorf("read binary: %w", err)
	}
	// Every PE image starts with the DOS header's "MZ". Anything else is a
	// binary built for another OS, which core on Windows could never start.
	if !bytes.HasPrefix(binary, []byte("MZ")) {
		return "", fmt.Errorf("%w: %s", errNotPE, o.binary)
	}
	install, uninstall, err := readScripts(o.scripts)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := writeZip(&buf, contents(m, binary, manifest, install, uninstall)); err != nil {
		return "", err
	}
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	path := filepath.Join(o.out, fmt.Sprintf("%s_%s_windows_%s.zip", m.ID, m.Version, o.arch))
	// 0644: a package for others to read and install, not a secret.
	//nolint:gosec // G306: see above
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return "", fmt.Errorf("write package: %w", err)
	}
	return path, nil
}

// writeZip writes entries to w in order, each deflated and stamped modTime.
func writeZip(w io.Writer, entries []entry) error {
	zw := zip.NewWriter(w)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: modTime}
		h.SetMode(0o644)
		f, err := zw.CreateHeader(h)
		if err == nil {
			_, err = f.Write(e.data)
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("write zip: %w", err)
	}
	return nil
}
