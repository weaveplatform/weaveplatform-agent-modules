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
| clipboard | stat, get, set; content over 256 KiB streams in chunks | `weave-linux-clipboard`: wl-clipboard or xclip | `weave-macos-clipboard`: NSPasteboard | `weave-windows-clipboard`: the Win32 clipboard |
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
go.work                    the workspace: sdk, every module and packaging/moduledeb (committed)
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
2. Every platform the manifest lists is built with `CGO_ENABLED=0 go build -trimpath`, as
   `<id>-<goos>-<goarch>[.exe]`.
3. A copy of the manifest is stamped with each artifact's `sha256` digest and size. The
   binaries and that sidecar are pushed with ORAS to
   `ghcr.io/weaveplatform/weaveplatform-modules/<id>:<version>`.
4. The same files are attached to the module's GitHub release.
5. A `module-published` dispatch asks weaveplatform-release-channels for a promotion PR, with a
   token minted from the org App (`RP_APP_ID`, `RP_APP_PRIVATE_KEY`) or, failing that,
   `RELEASE_PLEASE_PAT`. A failed dispatch fails the job; re-run it with
   `workflow_dispatch` and the tag.

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
- `Depends: weave-agent`.

`ARCH` defaults to this machine's architecture and `MODULES` to every Linux module.
`moduledeb` refuses a manifest that does not declare `linux/<ARCH>`. It is stdlib Go, so
this works on macOS with no Debian tooling.

Core's `packaging/apt` builds the `weave-agent` package and the signed apt repository. Put
the module packages from `dist/` beside the `weave-agent` package and build one repository
from them, and a guest installs core and its modules from it with `apt-get install`.

## Contributing

Commits and PR titles follow Conventional Commits. See [CONTRIBUTING.md](CONTRIBUTING.md)
and [CLAUDE.md](CLAUDE.md).
