# weaveplatform-agent-modules

The module SDK and the capability modules for
[weaveplatform-agent-core](https://github.com/weaveplatform/weaveplatform-agent-core).

Core (`weave-agent`) is the one agent on a machine, whether that machine is a VM, a
container or a device. It owns the machine's identity, its connection to the host and the
supervision of signed module binaries, and it ships no modules. Everything it can do for a
host comes from the modules here: run a process, power off, read the clock, take an
inventory, sync a clipboard. Each module is one capability for one OS,
`weave-<linux|macos|windows>-<capability>`, and any weave product drives it.

| Where core runs | Driven by | Over the host channel |
|---|---|---|
| A VM, on a device or on a cloud host (Virtualization.framework, Hyper-V, QEMU/KVM) | the hypervisor host | virtio-serial, vsock or HvSocket |
| A container, on a device or in the cloud | the container runtime's host | a Unix socket |

## The Terraform model

The repositories split the way Terraform does:

| Terraform | Weave |
|---|---|
| `hashicorp/terraform`: owns the plugin protocol and keeps a private implementation of it | **weaveplatform-agent-core**: owns `proto/` and `schema/`, implements its side in `internal/`, ships no modules |
| `terraform-plugin-go`, `terraform-plugin-framework`, `terraform-plugin-testing` | **`sdk/`** here: the module side of the protocol, the module runtime and its test harness |
| providers, each released on its own | **`modules/`** here: one Go module and one release per capability per OS |
| the Terraform Registry | **weaveplatform-release-channels**: the signed release channels (`stable`, pinned snapshots) that say which module and image versions devices run |

Dependencies point one way. Every module depends on the sdk, and nothing here depends on
core: core and the sdk agree on the wire, not on a Go package. The sdk generates its own
copy of the protocol from core's `proto/` at the core release in
[`.github/agent-core-version`](.github/agent-core-version), and CI runs a module built on
the sdk under that released `weave-agent` on Linux, macOS and Windows. See
[ADR 0002](docs/decisions/0002-module-sdk-terraform-model.md) and
[writing a module](sdk/docs/writing-a-module.md).

## Capabilities

The host addresses a capability, never an OS. Every OS variant of a capability declares the
same manifest `address`, `weave.<capability>`, and core routes the channel's traffic for that
address to whichever variant is installed. Ops are `weave.<capability>.<op>`, and a reply is
the op's kind plus `.result`. See
[ADR 0001](docs/decisions/0001-capability-modules-per-os.md).

| Capability | Ops | Linux | macOS | Windows |
|---|---|---|---|---|
| presence | hello (answered before authentication), inventory | `weave-linux-presence`: x/sys/unix, procfs, DMI | `weave-macos-presence`: Foundation, IOKit | `weave-windows-presence`: registry, SMBIOS |
| exec | start a process with pipes or a terminal; stdin, resize, signal | `weave-linux-exec`: pipes or a pseudo-terminal | `weave-macos-exec`: pipes or a pseudo-terminal | `weave-windows-exec`: pipes or ConPTY |
| power | shutdown, restart | `weave-linux-power`: systemctl or shutdown | `weave-macos-power`: shutdown | `weave-windows-power`: InitiateShutdown |
| time | get, set | `weave-linux-time`: clock_settime | `weave-macos-time`: settimeofday | `weave-windows-time`: SetSystemTime |
| metrics | sample CPU, load, memory, swap, disk, process count | `weave-linux-metrics`: kernel load and memory counters, procfs, statfs | `weave-macos-metrics`: Mach host statistics, libproc, sysctl | `weave-windows-metrics`: system times, memory status, disk space |
| clipboard | stat, get, set of every [canonical format](docs/clipboard.md) at once; content over 256 KiB streams in chunks | `weave-linux-clipboard`: Wayland data control or the X11 selection, in Go (wl-clipboard as a fallback) | `weave-macos-clipboard`: NSPasteboard | `weave-windows-clipboard`: the Win32 clipboard |
| session | current, list, lock; `weave.session.changed` events | `weave-linux-session`: logind, loginctl | `weave-macos-session`: SystemConfiguration, IOConsoleUsers (no lock) | `weave-windows-session`: WTS (no lock) |
| display | list modes and scale, set resolution and scale | `weave-linux-display`: wlr-randr or xrandr | `weave-macos-display`: CoreGraphics | `weave-windows-display`: ChangeDisplaySettingsEx (scale read-only) |

A variant answers `unsupported` (`weavewire.CodeUnsupported`) for an op its OS cannot
perform rather than imitating it. `sdk/weavewire` also reserves the names network, files,
shares, tunnel, provision, freeze, disk, logs, software, tools and osquery, so no other
module claims their addresses; each gets its ops when its modules are written.

Clipboard and display run in the session of the user at the physical console. With nobody
logged in there, core holds those modules in `waiting-for-session`, and `weaveclient`
reports a call to them as `weaveclient.ErrNoSession`: at once under weave-agent v0.9.2 and
later, which says why it could not deliver the call, and after `Options.SessionTimeout`
under an older core, which says nothing. Session runs as system and always answers, so
`SessionCurrent` tells "nobody is logged in" from "no module installed".

### Host side

```go
// conn is whatever reaches core directly: an HvSocket or vsock net.Conn, a Unix
// socket into a container, or weaveclient.Pipes(r, w) over a console pipe pair.
client := weaveclient.New(ctx, conn, weaveclient.Options{})
_ = client.Authenticate(ctx, channelKey)      // the machine's channel key; hello works before this
inv, _ := client.Inventory(ctx)               // → weave.presence
s, _ := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"uname", "-a"}})

// Which modules the guest has (weave-agent v0.9.2 and later): ask once, follow changes,
// and gate on the cached copy. An older core never answers: ErrRegistryUnsupported.
_, _ = client.Modules(ctx)
client.OnModulesChanged(func(s weaveclient.ModulesSnapshot) { /* revision only rises */ })
if client.Installed(weavewire.Clipboard.Address()) { /* ... */ }

// A call core cannot deliver fails at once, saying why, instead of timing out.
if _, err := client.Shutdown(ctx, "bye"); errors.Is(err, weaveclient.ErrModuleNotInstalled) {
	// no power module in this guest
}
```

## Layout

```
go.work                    the workspace: sdk, every module and the packaging tools (committed)
sdk/                       github.com/weaveplatform/weaveplatform-agent-modules/sdk
  modulesdk/ testkit/      the module runtime (Serve) and StubCore, a stand-in core for a built binary
  protocol/                handshake, ipc, hvchannel framing, manifest types: the hand-written wire
  gen/go/weave/agent/v1/   generated from agent-core's proto/ at .github/agent-core-version (make sdk-gen)
  config/ platform/ werror/ wlog/
  weavewire/               capability names and addresses, op kinds, payloads, stream chunks
  weavemodule/             the capability runtime: one Service per module, parity and manifest checks
  weaveagent/              dispatch and event emission under weavemodule
  weaveclient/             the host client: typed calls, exec sessions, auth, the module registry, pluggable transport
  weave<capability>/       the OS-neutral service for each capability; a module supplies its OS backend
  weavepolicy/             exec policy and audit records
  internal/compatfixture/  the module CI runs under the released weave-agent
  docs/writing-a-module.md
modules/weave-<os>-<capability>/
                           one Go module per capability per OS, built only for its OS
packaging/moduledeb/       turns a built Linux module into a .deb for local bring-up
packaging/modulepkg/       turns a built macOS module into a .pkg for local bring-up
packaging/modulezip/       turns a built Windows module into a .zip with install.ps1 and uninstall.ps1
tools/                     pinned developer tools (go-test-coverage, govulncheck); not in go.work
docs/decisions/            ADR 0001 (capability modules per OS), ADR 0002 (the sdk and the Terraform model)
```

A module is a `main.go` and an OS backend:

```go
modulesdk.Serve(weavemodule.New(
    weavemodule.ModuleID("linux", weavewire.Exec), // weave-linux-exec
    weaveexec.NewService(weaveexec.UnixStarter{}),
))
```

Its tests call `weavemodule.CheckParity` (it serves exactly its capability's ops) and
`weavemodule.CheckManifest` (its manifest declares the capability's address and its one OS).

## Building and testing

The workspace is the default. From the repository root:

```
make test                  # every module for this OS: race, shuffle, a coverage profile per module
make cover                 # each module's .testcoverage.yml (≥95% total, ≥90% per package)
make lint                  # golangci-lint over every module, a capability module as its own OS
make vet-all-os            # go vet for linux, darwin and windows
make standalone            # every module with GOWORK=off: its own go.mod, the sdk by replace
make vuln                  # govulncheck, as each OS a module builds for
make tidy                  # go mod tidy every module and go work sync
make gate                  # vet-all-os, lint, test, standalone, cover and vuln, in order
make sdk-gen               # regenerate sdk/gen from agent-core's proto/ at .github/agent-core-version
make compat AGENT_DIR=...  # run the sdk's compat fixture under an extracted weave-agent release
```

A capability module builds only for its own OS, so `make test` and `make cover` skip the
other OSes' modules. The quality gate runs every module on each OS it targets, merges the
per-OS coverage profiles before it enforces each module's gate, and also checks that:

- `sdk/gen` is exactly what core's `proto/` generates at the pinned release;
- neither the sdk nor any module depends on `weaveplatform-agent-core`;
- a module built on the sdk runs under the released `weave-agent` on Linux, macOS and
  Windows, until core's `weavectl` reports it running and healthy;
- every `go.mod` is tidy and joined to `go.work`.

`Quality gate` is the single required check.

## Releasing

release-please runs in manifest mode with one component per released Go module:

| Component | Tag | Also bumps |
|---|---|---|
| `sdk` | `sdk/vX.Y.Z` | |
| `modules/weave-<os>-<capability>` | `modules/weave-<os>-<capability>/vX.Y.Z` | `$.version` in the module's `module.manifest.json` |

The first release of every component is 0.2.0, set once by `initial-version` in
[`release-please-config.json`](release-please-config.json). The Go checksum database and
GHCR already hold 0.1.x versions of these module paths and image names, and a version they
hold can never be published again with different content. release-please uses
`initial-version` only while a component's entry in
[`.release-please-manifest.json`](.release-please-manifest.json) is `0.0.0`, so it applies
to the first release alone; each `module.manifest.json` already declares 0.2.0 to match.
After that, versions follow Conventional Commits: before 1.0.0 a feature bumps the patch
version and a breaking change the minor.

A module's tag runs [`module-release.yml`](.github/workflows/module-release.yml):

1. The tag names the module directory. The manifest's `id` must equal the directory name and
   its `version` the tag's version, or nothing is published.
2. Every platform the manifest lists is built on Linux with
   `CGO_ENABLED=0 go build -trimpath`, as `<id>-<goos>-<goarch>[.exe]`.
3. A module with darwin platforms is signed and notarised on macOS, and a module with
   windows platforms is Authenticode-signed on Windows (both below). Linux binaries go
   straight to the next step.
4. A copy of the manifest is stamped with each artifact's `sha256` digest and size, taken
   after signing, so a channel pins the signed binary. The binaries and that sidecar are
   pushed with ORAS to `ghcr.io/weaveplatform/weaveplatform-modules/<id>:<version>`. A
   darwin or windows binary with no signed copy fails the release.
5. The same files, and a macOS module's signed `.pkg` or a Windows module's `.zip`, are
   attached to the module's GitHub release.
6. A `module-published` dispatch asks weaveplatform-release-channels for a promotion PR, with a
   token minted from the org App (`RP_APP_ID`, `RP_APP_PRIVATE_KEY`) or, failing that,
   `RELEASE_PLEASE_PAT`. A failed dispatch fails the job; re-run it with
   `workflow_dispatch` and the tag.

### Signed and notarised macOS modules

A release `weave-agent` on macOS launches a module only if
`codesign --verify --strict=all` accepts it against
`anchor apple generic and certificate leaf[subject.OU] = "<team>"`, where `<team>` is the
manifest's `signing.apple_team_id`, and its `TeamIdentifier` is that team: an unsigned,
ad-hoc or self-signed binary is refused. Every `weave-macos-*` manifest pins the
weaveplatform team, `5GM6DW5337`.

The release runs as three jobs: **build** (Linux) → **sign-darwin** (macOS) → **publish**
(Linux). Only `sign-darwin` holds the Apple certificates and notary key, and its token is
read-only; only `publish` can write packages and releases, and it never sees a signing
secret. `sign-darwin`:

- imports the Developer ID Application and Installer identities into a throwaway keychain,
  and refuses a manifest whose `apple_team_id` is not the org's `APPLE_TEAM_ID`;
- signs each darwin binary with
  `codesign --force --sign "Developer ID Application: … (5GM6DW5337)" --options runtime --timestamp`
  and the identifier `run.weaveplatform.module.<id>`;
- notarises it (a `ditto -c -k` zip through `notarytool submit --wait`), printing the
  notary log and failing on anything but `Accepted`. A bare Mach-O cannot hold a stapled
  ticket, so Gatekeeper checks its notarisation online;
- verifies it with core's exact requirement, and checks `TeamIdentifier`, the secure
  timestamp and the hardened runtime in `codesign -dv`;
- packages it with `modulepkg`, signed with the Developer ID Installer identity, notarises
  and staples the `.pkg`, and checks it with `stapler validate` and
  `spctl -a -vv -t install`;
- deletes the keychain and the `.p8`, whatever happened.

Each notarisation's submission id and status are in the run's summary. Check a released
module without installing it:

```
codesign -dv weave-macos-clipboard-darwin-arm64            # TeamIdentifier=5GM6DW5337, flags=0x10000(runtime)
codesign --verify --strict=all \
  -R='anchor apple generic and certificate leaf[subject.OU] = "5GM6DW5337"' \
  weave-macos-clipboard-darwin-arm64                         # core's own check
spctl -a -vv -t install weave-macos-clipboard_*_darwin_arm64.pkg   # accepted, Notarized Developer ID
xcrun stapler validate weave-macos-clipboard_*_darwin_arm64.pkg
```

To prove the signing path on a branch before it merges, dispatch the workflow with
`sign_check` set to a macOS module (`gh workflow run module-release.yml --ref <branch>
-f sign_check=weave-macos-clipboard`). It builds, signs, notarises and packages that
module from the branch and uploads the binary (`darwin-signed`) and package
(`darwin-pkg`) as workflow artifacts; nothing is pushed, released or dispatched.

### Signed Windows modules

A release `weave-agent` on Windows launches a module only if `WinVerifyTrust` accepts its
Authenticode signature, chain included, and the leaf certificate's SHA-1 thumbprint is
the manifest's `signing.authenticode_thumbprint`. Every `weave-windows-*` manifest pins
the weaveplatform code-signing certificate:

| | |
|---|---|
| Subject | `CN=weaveplatform code signing, O=weaveplatform` (self-signed, code signing EKU, valid to 2031-10-04) |
| SHA-1 thumbprint | `A6A3936288B9409ED7A3458CF81014A77AB59B51` |
| Manifest pin | `"signing": {"authenticode_subject": "weaveplatform code signing", "authenticode_thumbprint": "A6A3936288B9409ED7A3458CF81014A77AB59B51"}` |

The certificate is self-signed, so nothing trusts it by default: core's Windows installer
adds the public certificate to the guest's machine `Root` and `TrustedPublisher` stores,
and only then does `WinVerifyTrust` accept a module. The subject is pinned as well as the
thumbprint because core releases to date refuse a manifest without
`authenticode_subject` before they read the thumbprint; when both are present the
thumbprint is what is compared.

The organisation holds the signing material:

| Name | Kind | What |
|---|---|---|
| `WINDOWS_CODESIGN_PFX` | secret | the certificate and private key, as a base64 PFX |
| `WINDOWS_CODESIGN_PFX_PASSWORD` | secret | the PFX's password |
| `WINDOWS_CODESIGN_THUMBPRINT` | variable | the pinned thumbprint |
| `WINDOWS_CODESIGN_CERT` | variable | the public certificate, PEM (or that PEM base64-encoded) |

The release's **sign-windows** job runs on `windows-latest`, between **build** and
**publish**, for a module with windows platforms. It is the only job given the PFX secrets,
and its token is read-only. It uses `signtool` rather than `osslsigncode` on Linux, because
the job must run `WinVerifyTrust`, which exists only on Windows, and signtool is
Microsoft's own signer, already on the runner, and signs the PowerShell scripts as well.
[`.github/scripts/authenticode.ps1`](.github/scripts/authenticode.ps1) holds the signing
and checks. The job:

- refuses a manifest whose `authenticode_thumbprint` is not `WINDOWS_CODESIGN_THUMBPRINT`,
  a `WINDOWS_CODESIGN_CERT` or PFX holding another certificate, and missing secrets. A
  real release then fails rather than publish binaries core would refuse;
- signs each windows binary with `signtool sign /fd SHA256`, then adds an RFC 3161
  timestamp with `signtool timestamp /tr http://timestamp.digicert.com /td SHA256`,
  retried with backoff and alternating with Sectigo's server, so the signature outlives
  the certificate;
- checks with `Get-AuthenticodeSignature` that each binary is signed by the pinned
  thumbprint and timestamped;
- adds the public certificate to the runner's machine `Root` and `TrustedPublisher`
  stores, as core's installer does on a guest, then requires `Get-AuthenticodeSignature`
  to report `Valid` and `signtool verify /pa` (WinVerifyTrust, the Authenticode policy
  core checks) to accept each binary with a SHA-256 file digest;
- packages each binary with `modulezip`, with Authenticode-signed `install.ps1` and
  `uninstall.ps1`, then installs and removes each zip into a scratch directory under
  `-ExecutionPolicy AllSigned`;
- deletes the PFX and removes the certificate from the stores, whatever happened.

Check a released module on Windows, after trusting the certificate as a guest does:

```
Get-AuthenticodeSignature weave-windows-clipboard-windows-amd64.exe   # Valid, signer thumbprint A6A3…9B51
signtool verify /pa /v weave-windows-clipboard-windows-amd64.exe      # Hash of file (sha256), timestamp
```

or on any machine with `osslsigncode`:

```
osslsigncode verify -CAfile weave-codesign.crt -in weave-windows-clipboard-windows-amd64.exe
```

To prove the Windows signing path on a branch, dispatch the workflow with `sign_check` set
to a Windows module (`gh workflow run module-release.yml --ref <branch>
-f sign_check=weave-windows-clipboard`). It builds, signs, verifies and packages that
module and uploads the binaries (`windows-signed`) and zips (`windows-zip`) as workflow
artifacts; nothing is pushed, released or dispatched.

A module requires the sdk through `replace => ../../sdk`, so a module release always carries
the sdk at the same commit. The `sdk/vX.Y.Z` tags are for module authors outside this
repository, who require the sdk like any Go module.

## Local bring-up packages

Devices install modules from a channel. To bring up a Linux guest from a local apt
repository instead, build Debian packages of the Linux modules:

```
make debs ARCH=arm64 MODULES="weave-linux-presence weave-linux-exec"
```

Each module is built as the release pipeline builds it and packaged by
[`packaging/moduledeb`](packaging/moduledeb) into `dist/<id>_<version>_<arch>.deb`:

- `/usr/lib/weave/modules/<id>/<id>`, 0755, root-owned: the path core's Linux layout runs;
- `/usr/lib/weave/modules/<id>/module.manifest.json`, 0644, root-owned;
- `Depends: weave-agent`;
- a `postinst` (on `configure`) and a `postrm` (on `remove` and `purge`) that run
  `systemctl reload weave-agent` when systemd is running and the unit is active.

So `dpkg -i`, an upgrade or `dpkg -r` of a module package takes effect at once on a
running guest: weave-agent starts, replaces or stops that module without restarting
itself or the other modules. The scripts never fail the package operation, and do
nothing where systemd is not running, as in a chroot or an image build; there,
weave-agent (v0.9.3 or later) also watches its modules directory, so a module installed
before it starts, or behind its back, is still picked up. The scripts are in
[`packaging/moduledeb/scripts`](packaging/moduledeb/scripts).

`ARCH` defaults to this machine's architecture and `MODULES` to every Linux module.
`moduledeb` refuses a manifest that does not declare `linux/<ARCH>`. It is stdlib Go, so
this works on macOS with no Debian tooling.

Core's `packaging/apt` builds the `weave-agent` package and the signed apt repository. Put
the module packages from `dist/` beside the `weave-agent` package and build one repository
from them, and a guest installs core and its modules from it with `apt-get install`.

### macOS

The macOS counterpart builds installer packages of the macOS modules, for a guest running
core's `weave-agent` package (agent-core
[`docs/macos-package.md`](https://github.com/weaveplatform/weaveplatform-agent-core/blob/main/docs/macos-package.md)).
It runs on macOS, since it needs `pkgbuild`:

```
make pkgs MODULES="weave-macos-presence weave-macos-exec"
```

Each module is built as the release pipeline builds it and packaged by
[`packaging/modulepkg`](packaging/modulepkg) into `dist/<id>_<version>_darwin_arm64.pkg`, a
flat component package with the identifier `run.weaveplatform.module.<id>`:

| Path | What | Owner, mode |
|---|---|---|
| `/usr/local/libexec/weave/modules/<id>/<id>` | the module binary: the path core's macOS layout runs | root:wheel 0755 |
| `/usr/local/libexec/weave/modules/<id>/module.manifest.json` | its manifest | root:wheel 0644 |
| `/usr/local/libexec/weave/uninstall.d/<id>.sh` | its uninstaller | root:wheel 0755 |

Every directory in the payload is root:wheel 0755, the mode macOS and core's package already
give the ones that exist, because installer applies a payload directory's mode to an
existing one. Install one in the guest with
`sudo installer -pkg <id>_<version>_darwin_arm64.pkg -target /`.
`PKG_ARCH` is `arm64` unless set, and `modulepkg` refuses a manifest that does not declare
`darwin/<arch>`.

- **Signing.** `make pkgs` builds unsigned binaries in unsigned packages, which a release
  `weave-agent` refuses to run; they suit a guest running a `dev` build of core. To sign
  the package, pass a Developer ID Installer identity in your keychain with `-sign` or
  `WEAVE_PKG_SIGN_IDENTITY` (`WEAVE_PKG_SIGN_IDENTITY="Developer ID Installer: …" make pkgs`),
  and sign the binary first with `codesign --options runtime --timestamp`. The packages
  attached to each module's GitHub release are built by the release from the signed,
  notarised binary, signed with the weaveplatform Installer identity, notarised and
  stapled: install those on a guest running a released `weave-agent`.

- **postinstall** runs `launchctl kill HUP system/run.weaveplatform.agent` when that daemon
  is loaded, so weave-agent starts or replaces the module at once. It does nothing when the
  daemon is not loaded or the package is installed onto another volume (an image build),
  and it never fails the install. weave-agent's kqueue watch on its modules directory and
  its minute rescan cover those cases.
- **Removal.** macOS has no package removal, so the package carries its own uninstaller:
  `sudo /usr/local/libexec/weave/uninstall.d/<id>.sh` removes the binary and manifest, the
  module directory if nothing else is in it, the uninstaller itself and the package
  receipt (`pkgutil --forget run.weaveplatform.module.<id>`), then sends the same reload
  so weave-agent stops the module at once. `WEAVE_PKG_DRYRUN=1` prints what it would do.

The scripts are in [`packaging/modulepkg/scripts`](packaging/modulepkg/scripts). Check a
build without installing it:

```
pkgutil --payload-files dist/weave-macos-exec_*_darwin_arm64.pkg
pkgutil --expand dist/weave-macos-exec_*_darwin_arm64.pkg /tmp/x && lsbom -p MUGf /tmp/x/Bom
WEAVE_PKG_DRYRUN=1 sh packaging/modulepkg/scripts/postinstall pkg / /   # prints, changes nothing
```

A package built on a Mac whose shell tags new files with `com.apple.provenance` lists
`._*` entries in its payload. Those are the build machine's extended attributes, which
installer restores as attributes rather than files, as with core's own package.

### Windows

The Windows counterpart builds a zip of each Windows module that installs itself, for a
guest running core's `WeaveAgent` service (agent-core
[`docs/windows-install.md`](https://github.com/weaveplatform/weaveplatform-agent-core/blob/main/docs/windows-install.md)).
`modulezip` is stdlib Go, so this runs on any OS:

```
make zips MODULES="weave-windows-presence weave-windows-clipboard" ZIP_ARCH=arm64
```

Each module is built as the release pipeline builds it and packaged by
[`packaging/modulezip`](packaging/modulezip) into
`dist/<id>_<version>_windows_<arch>.zip`. `ZIP_ARCH` is `amd64` unless set, and `modulezip`
refuses a manifest that does not declare `windows/<arch>`, and a binary that is not a
Windows executable. A zip and a script rather than an MSI, because that is how core itself
installs on Windows (its release zip and `install.ps1`). The media and unattend steps that
install core then install a module the same way, unattended, and building one needs no
WiX or Windows machine.

| In the zip | Installed as |
|---|---|
| `install.ps1` | (run from the unpacked zip) |
| `uninstall.ps1` | `%ProgramFiles%\Weave\uninstall.d\<id>.ps1` |
| `module\<id>.exe` | `%ProgramFiles%\Weave\modules\<id>\<id>.exe`: the name core's discovery looks for |
| `module\module.manifest.json` | `%ProgramFiles%\Weave\modules\<id>\module.manifest.json` |

`%ProgramFiles%\Weave\modules` is the package-owned tree core's service runs modules from
(`--modules-dir`, the counterpart of `/usr/lib/weave/modules`). Install from an elevated
prompt, the specialize pass or a FirstLogonCommand:

```
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\install.ps1
```

- **Install.** Each file is copied beside its destination and renamed over it, the binary
  first and the manifest last. Core reads a directory with no manifest as no module, so a
  rescan mid-install never finds a manifest without its binary. Core runs every Windows
  module from a copy in its exec directory, so an upgrade replaces the installed binary
  while the module runs. `-InstallDir` installs into another core install directory.
- **Reload.** Windows has no SIGHUP and core has no directory watch there yet, so the
  script runs `%ProgramFiles%\Weave\weavectl.exe reload`, and weave-agent starts or
  replaces the module at once. A failed reload, or no weavectl, only warns: core's periodic
  rescan (a minute by default) finds the module anyway. `-NoReload` skips it, as for an
  image build.
- **Removal.** `powershell.exe -NoProfile -ExecutionPolicy Bypass -File
  "%ProgramFiles%\Weave\uninstall.d\<id>.ps1"` removes the manifest, the binary, the
  module directory if nothing else is left in it (an operator's `config.json` stays) and
  itself, then reloads weave-agent the same way so it stops the module. `uninstall.ps1` in
  the unpacked zip does the same for its module.
- **Signing.** `make zips` packages unsigned binaries and scripts, which a release
  `weave-agent` refuses to run; they suit a guest running a `dev` build of core. The zips
  attached to each module's GitHub release carry the Authenticode-signed binary and
  Authenticode-signed scripts. Once core's installer has trusted the certificate, they also
  run under `-ExecutionPolicy AllSigned`.
- **Re-installing core.** Core's own `install.ps1` replaces `%ProgramFiles%\Weave\modules`
  wholesale when its media carries a `modules\` tree. Install module zips after core, or
  put the modules in core's media.

The scripts are in [`packaging/modulezip/scripts`](packaging/modulezip/scripts), and its
tests run them under Windows PowerShell and PowerShell 7 wherever those are installed.

## Contributing

Commits and PR titles follow Conventional Commits. See [CONTRIBUTING.md](CONTRIBUTING.md)
and [CLAUDE.md](CLAUDE.md).
