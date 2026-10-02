# weaveplatform-agent-modules

The guestweave guest capabilities, built as
[weaveplatform-agent-core](https://github.com/weaveplatform/weaveplatform-agent-core)
modules: one module per capability per guest OS
(`guestweave-<linux|macos|windows>-<capability>`). Core runs inside the guest, owns the
hypervisor channel and authenticates the host on it, and supervises the signed module
binaries; each module serves one capability's ops.

Together they replace every in-guest agent the guestweave CLIs carry today:
`weave-guestd` and the Tart guest agent in guestweave-cli-macos, `weave-guestd.exe`, the
native Linux `weave-agent` and the external tools agent in guestweave-cli-windows, and the
combined `guestweave` module from guestweave-agent.

## Addressing

The host addresses a capability, never an OS. Every OS variant of a capability declares
the same manifest `address`, `guestweave.<capability>`, and core routes the channel's
traffic for that address to whichever variant is installed. Ops are
`guestweave.<capability>.<op>`, and a reply is the op's kind plus `.result`. A CLI
therefore runs the same `weave exec` against a Linux, macOS or Windows guest.
See [docs/decisions/0001-capability-modules-per-os.md](docs/decisions/0001-capability-modules-per-os.md).

## Capabilities

| Capability | Ops | Session | Migrated from / replaces | OS notes |
|---|---|---|---|---|
| presence | hello (pre-auth), heartbeat, readiness, inventory (IPs, OS, hostname, module set + versions) | system | guestweave-agent hello/inventory; weave-guestd hello; Tart `ResolveIP` | Readiness replaces "IP resolved = ready" |
| exec | exec pipe/TTY, stdin, resize, signal, wait | system (+ run-as user) | guestweave-agent exec; Tart `Exec`; Windows native exec | Windows ConPTY |
| power | shutdown, restart | system | weave-guestd power; native shutdown; guestweave-agent | |
| clipboard | stat, get, set | per-user-console | macOS + Windows `modules/clipboard` | darwin JXA+pbcopy; linux wl-clipboard/xclip; windows winclip |
| time | get, set, sync after resume | system | guestweave-agent | |
| metrics | cpu, memory, disk, network | system | guestweave-agent | |
| network | apply static NIC config, list interfaces | system | Windows native `modify` (netlink) | macOS `networksetup`; windows netsh/WMI |
| files | read/write/stat/list/remove, chunked transfer | system (+ run-as) | SFTP use in both CLIs | |
| shares | mount/unmount host shares | system | Windows native 9p-over-vsock | linux virtiofs/9p; macOS VZ share tags; windows virtiofs/SMB |
| tunnel | TCP forward over the channel (host→guest port) | system | (new) | |
| provision | first-boot: hostname, users/passwords, SSH authorized_keys, setup-complete marker | system | cloud-init seed user creation; unattend `WeaveSetupComplete`; macOS OCR presets | Linux defers to cloud-init when present |
| session | console user, autologon, lock/unlock, session change events | system | Windows unattend autologon | Feeds clipboard's per-user launch |
| freeze | freeze/thaw filesystems for snapshots | system | (new) | linux fsfreeze; windows VSS; macOS sync-only |
| disk | grow partition + filesystem after resize | system | (new) | linux growpart/resize2fs/xfs_growfs; macOS `diskutil apfs resizeContainer`; windows `Resize-Partition` |
| display | set/resize resolution | per-user-console | (new) | linux xrandr/wlr; windows `ChangeDisplaySettingsEx` |
| logs | stream/tail journal, unified log, Event Log | system | (new) | |
| software | installed packages + pending updates (SBOM input) | system | (new) | dpkg/rpm; pkgutil/brew; winget/registry/KB |
| tools | `tools` list, `call <tool> <json>` | system | external guestweave-agent CLI | |
| osquery | run SQL queries, list tables | system | (new) | osquery-go against the osqueryd extension socket |

`pkg/guestwire` defines the wire contract (ops and payloads) for presence, exec, power,
time and metrics today. The other names are reserved there, and each one gets its ops
when its module lands. A variant answers `unsupported` (`guestwire.CodeUnsupported`) for
an op its OS cannot perform, rather than imitating it.

## Layout

```
go.work                    the workspace: every module below joins it (committed)
pkg/                       github.com/weaveplatform/weaveplatform-agent-modules/pkg
  guestwire/               capability names and addresses, op kinds, payloads, stream chunks
  guesthost/               host client for the CLIs: typed calls, exec sessions, auth, pluggable transport
  guestmodule/             the module runtime: one capability Service per module, parity and manifest checks
    guestmoduletest/       in-memory host and a stand-in core for testing services and hosts
  guestagent/              dispatch and event emission under guestmodule
  guestpresence/ guestexec/ guestpower/ guesttime/ guestmetrics/
                           OS-neutral capability services; each per-OS module supplies the backend
  guestpolicy/             exec policy and audit records
modules/                   guestweave-<os>-<capability>/ (next: presence, exec, power, time, metrics)
tools/                     pinned developer tools (go-test-coverage, govulncheck); not in go.work
```

A per-OS module is a `main.go` and an OS backend:

```go
modulesdk.Serve(guestmodule.New(
    guestmodule.ModuleID("linux", guestwire.Exec), // guestweave-linux-exec
    guestexec.NewService(guestexec.UnixStarter{}),
))
```

Its tests call `guestmodule.CheckParity` (it serves exactly its capability's ops) and
`guestmodule.CheckManifest` (its manifest declares the capability's address and its
one OS).

## Host side

```go
// conn is whatever reaches the guest: the CLI's HvSocket net.Conn on Windows,
// or guesthost.Pipes(r, w) over a Virtualization.framework console pipe pair.
client := guesthost.New(ctx, conn, guesthost.Options{})
_ = client.Authenticate(ctx, vmKey)           // the VM's Ed25519 key; hello works before this
inv, _ := client.Inventory(ctx)               // → guestweave.presence
s, _ := client.Exec(ctx, guestwire.ExecRequest{Argv: []string{"uname", "-a"}})
```

`guesthost.Dial` takes a `Dialer` and authenticates in the same step.

## Building and testing

The workspace is the default. From the repository root:

```
go test ./pkg/...          # workspace mode: modules resolve each other locally
make test                  # every module, race + shuffle, with a coverage profile for this OS
make cover                 # each module's .testcoverage.yml (≥95% total, ≥90% per package)
make lint                  # golangci-lint, blocking in CI
make vet-all-os            # go vet for linux, darwin and windows
make standalone            # every module with GOWORK=off, against released dependencies
make gate                  # all of the above, as CI runs them
```

Without the workspace, inside a module: `cd pkg && GOWORK=off go test ./...`. CI runs both
ways on Linux, macOS and Windows, and merges the three OS coverage profiles before it
enforces each module's gate.

## Releases

release-please runs in manifest mode with one component per module. `pkg` is released as
`pkg/vX.Y.Z` (starting at 0.1.0), and each capability module will get its own
`<path>/vX.Y.Z` component as it lands. Commit messages and PR titles follow Conventional
Commits.
