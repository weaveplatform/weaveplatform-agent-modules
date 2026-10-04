// Command modulepkg packages one built weave module as a macOS installer
// package, for local bring-up of a macOS guest: the counterpart of moduledeb.
//
// A capability module is a binary and its module.manifest.json, nothing more.
// This turns that pair into the flat component package core's macOS layout
// expects: the binary at /usr/local/libexec/weave/modules/<id>/<id> (core
// looks for exactly that name) and the manifest beside it, root:wheel, in a
// package whose identifier is run.weaveplatform.module.<id>. Its postinstall
// tells a loaded weave-agent daemon to reload, so installing or upgrading the
// package takes effect at once.
//
// macOS has no package removal, so the package also installs an uninstaller,
// /usr/local/libexec/weave/uninstall.d/<id>.sh, which removes the module's
// files, forgets the receipt and reloads weave-agent: what a Debian package's
// postrm does.
//
// The package is built by pkgbuild, so this runs on macOS only; it is stdlib
// Go and stages everything itself, so the only tool it needs is pkgbuild.
//
//	modulepkg -binary weave-macos-presence -manifest module.manifest.json -arch arm64 -out dist
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// The paths of core's macOS layout (agent-core docs/macos-package.md),
// relative to the payload root, which installs at /.
const (
	libexecDir   = "usr/local/libexec/weave"
	modulesDir   = libexecDir + "/modules"
	uninstallDir = libexecDir + "/uninstall.d"
)

// identifierPrefix names a module package's receipt. core's own package is
// run.weaveplatform.agent; a module's sits under the same reverse-DNS root so
// `pkgutil --pkgs='run\.weaveplatform\..*'` lists everything weave installed.
const identifierPrefix = "run.weaveplatform.module."

var (
	idRe      = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	// archs are the Go architectures a macOS module may be packaged for.
	archs = map[string]bool{"arm64": true, "amd64": true}
)

// The ways a package is refused. Each is wrapped with the detail that names
// the offending file or value.
var (
	errManifest    = errors.New("invalid module manifest")
	errArch        = errors.New("unsupported architecture")
	errNotDeclared = errors.New("platform not declared by the module")
	errNoPkgbuild  = errors.New("pkgbuild not found: macOS packages are built on macOS")
)

// The scripts every module package carries, as shellcheck reads them. The
// uninstaller is a template: @ID@ and @PKGID@ become the module's id and the
// package identifier.
var (
	//go:embed scripts/postinstall
	postinstall []byte
	//go:embed scripts/uninstall
	uninstallTemplate string
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options are what one package build needs.
type options struct {
	binary, manifest, arch, out string
	// pkgbuild is the tool run to build the package, found on PATH by
	// default; sign, when set, is the "Developer ID Installer" identity it
	// signs with.
	pkgbuild, sign string
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("modulepkg", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.binary, "binary", "", "the built module binary (required)")
	fs.StringVar(&o.manifest, "manifest", "", "the module's module.manifest.json (required)")
	fs.StringVar(
		&o.arch,
		"arch",
		"arm64",
		"Go architecture the binary was built for: arm64 or amd64",
	)
	fs.StringVar(&o.out, "out", ".", "directory to write the .pkg into")
	fs.StringVar(&o.pkgbuild, "pkgbuild", "pkgbuild", "the pkgbuild to run")
	fs.StringVar(&o.sign, "sign", os.Getenv("WEAVE_PKG_SIGN_IDENTITY"),
		"a Developer ID Installer identity to sign with (default $WEAVE_PKG_SIGN_IDENTITY)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if o.binary == "" || o.manifest == "" {
		fmt.Fprintln(stderr, "modulepkg: -binary and -manifest are required")
		return 2
	}
	path, err := build(o)
	if err != nil {
		fmt.Fprintln(stderr, "modulepkg:", err)
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
		if p.OS == "darwin" && p.Arch == arch {
			return m, raw, nil
		}
	}
	return m, nil, fmt.Errorf("%w: %s: %s does not declare darwin/%s",
		errNotDeclared, path, m.ID, arch)
}

// file is one payload or script file, its path relative to its tree.
type file struct {
	name string
	mode os.FileMode
	data []byte
}

// payload lists the files the package installs, relative to /.
func payload(m moduleManifest, binary, manifest []byte) []file {
	dir := modulesDir + "/" + m.ID
	return []file{
		{name: dir + "/" + m.ID, mode: 0o755, data: binary},
		{name: dir + "/module.manifest.json", mode: 0o644, data: manifest},
		{name: uninstallDir + "/" + m.ID + ".sh", mode: 0o755, data: []byte(uninstaller(m.ID))},
	}
}

// uninstaller is the module's uninstall script.
func uninstaller(id string) string {
	return strings.NewReplacer("@ID@", id, "@PKGID@", identifierPrefix+id).
		Replace(uninstallTemplate)
}

// build writes <id>_<version>_darwin_<arch>.pkg into o.out and returns its
// path.
func build(o options) (string, error) {
	if !archs[o.arch] {
		return "", fmt.Errorf("%w %q (want arm64 or amd64)", errArch, o.arch)
	}
	m, manifest, err := readManifest(o.manifest, o.arch)
	if err != nil {
		return "", err
	}
	binary, err := os.ReadFile(o.binary)
	if err != nil {
		return "", fmt.Errorf("read binary: %w", err)
	}
	pkgbuild, err := exec.LookPath(o.pkgbuild)
	if err != nil {
		return "", fmt.Errorf("%w (%w)", errNoPkgbuild, err)
	}

	work, err := os.MkdirTemp("", "modulepkg-")
	if err != nil {
		return "", fmt.Errorf("create work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	root, scripts := filepath.Join(work, "root"), filepath.Join(work, "scripts")
	if err := stage(root, payload(m, binary, manifest)); err != nil {
		return "", err
	}
	if err := stage(
		scripts,
		[]file{{name: "postinstall", mode: 0o755, data: postinstall}},
	); err != nil {
		return "", err
	}

	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	path := filepath.Join(o.out, fmt.Sprintf("%s_%s_darwin_%s.pkg", m.ID, m.Version, o.arch))
	args := []string{
		"--root", root, "--scripts", scripts,
		"--identifier", identifierPrefix + m.ID, "--version", m.Version,
		// The payload root is /, and installer applies a payload directory's
		// mode to one that already exists: "recommended" ownership makes
		// every entry root:wheel, and stage made every directory 0755, the
		// mode macOS and core's package already give each of them.
		"--install-location", "/", "--ownership", "recommended",
	}
	if o.sign != "" {
		args = append(args, "--sign", o.sign)
	}
	args = append(args, path)
	//nolint:gosec // G204: the tool the caller named
	cmd := exec.CommandContext(context.Background(), pkgbuild, args...)
	// Without this, pkgbuild carries a file's extended attributes (a
	// developer Mac's com.apple.provenance) as AppleDouble ._* entries.
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("pkgbuild: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
}

// chmod is a seam so tests can fail the one call staging cannot otherwise be
// made to fail on a directory it just created.
var chmod = os.Chmod

// stage writes files under dir, creating dir and every directory between at
// 0755 whatever the umask, and each file at exactly its mode.
func stage(dir string, files []file) error {
	mkdir := func(d string) error {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("stage %s: %w", d, err)
		}
		if err := chmod(d, 0o755); err != nil { //nolint:gosec // G302: a payload directory
			return fmt.Errorf("stage %s: %w", d, err)
		}
		return nil
	}
	if err := mkdir(dir); err != nil {
		return err
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.name))
		rel := ""
		for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(f.name)), "/") {
			rel = filepath.Join(rel, part)
			if err := mkdir(filepath.Join(dir, rel)); err != nil {
				return err
			}
		}
		if err := os.WriteFile(p, f.data, f.mode); err != nil {
			return fmt.Errorf("stage %s: %w", f.name, err)
		}
		if err := chmod(p, f.mode); err != nil {
			return fmt.Errorf("stage %s: %w", f.name, err)
		}
	}
	return nil
}
