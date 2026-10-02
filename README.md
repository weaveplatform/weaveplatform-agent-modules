# weaveplatform-agent-modules

Capability modules for [weaveplatform-agent-core](https://github.com/weaveplatform/weaveplatform-agent-core):
the functions a weave product needs to manage and control an operating system — run a
process, move a file, power off, read the clock, take an inventory, sync a clipboard.

Core is the universal device and container agent: one per machine, wherever that machine
is. It owns the machine's identity, its connections and the supervision of signed module
binaries; the modules here give it something to do. Each module is one capability for one
OS, `weave-<linux|macos|windows>-<capability>`, and any product in the family drives it —
hostweave placing jobs on cloud and device capacity, guestweave managing VMs on a device,
sightweave, appweave, or whatever comes next.

| Where core runs | Driven by | Over the host channel |
|---|---|---|
| A VM, on a device or on a cloud host (Virtualization.framework, Hyper-V, QEMU/KVM) | the hypervisor host: guestweave, or hostweave placing a job | virtio-serial, vsock or HvSocket |
| A container, on a device or in the cloud | the container runtime's host: hostweave placing a job | a Unix socket |

Whatever the machine, the host directly outside it drives the capabilities over core's
one authenticated host channel; core does the routing.

## Addressing

The host addresses a capability, never an OS. Every OS variant of a capability declares
the same manifest `address`, `weave.<capability>`, and core routes the channel's
traffic for that address to whichever variant is installed. Ops are
`weave.<capability>.<op>`, and a reply is the op's kind plus `.result`. A product
therefore runs the same `exec` against a Linux, macOS or Windows machine.
See [docs/decisions/0001-capability-modules-per-os.md](docs/decisions/0001-capability-modules-per-os.md).

## Capabilities

| Capability | Ops | Session | OS notes |
|---|---|---|---|
| presence | hello (answerable before authentication), heartbeat, readiness, inventory (addresses, OS, hostname, installed modules) | system | |
| exec | run a process with pipes or a terminal; stdin, resize, signal, wait | system (+ run-as user) | Windows: ConPTY |
| power | shutdown, restart | system | |
| clipboard | stat, get, set | per-user-console | macOS JXA + pbcopy; Linux wl-clipboard/xclip; Windows clipboard API |
| time | get, set, resync after resume | system | |
| metrics | CPU, memory, disk, network | system | |
| network | apply static interface configuration, list interfaces | system | Linux netlink; macOS `networksetup`; Windows netsh/WMI |
| files | read, write, stat, list, remove; chunked transfer | system (+ run-as user) | |
| shares | mount and unmount host shares | system | Linux virtiofs/9p; macOS virtiofs tags; Windows virtiofs/SMB |
| tunnel | forward a TCP port over core's connection | system | |
| provision | first boot: hostname, users, SSH keys, setup-complete marker | system | Linux defers to cloud-init when present |
| session | console user, autologon, lock/unlock, session changes | system | |
| freeze | freeze and thaw filesystems around a snapshot | system | Linux fsfreeze; Windows VSS; macOS sync only |
| disk | grow the partition and filesystem after a resize | system | Linux growpart + resize2fs/xfs_growfs; macOS `diskutil apfs resizeContainer`; Windows `Resize-Partition` |
| display | set and resize the resolution | per-user-console | Linux xrandr/wlr; Windows `ChangeDisplaySettingsEx` |
| logs | stream and tail the journal, unified log or Event Log | system | |
| software | installed packages and pending updates (SBOM input) | system | dpkg/rpm; pkgutil/brew; winget/registry |
| tools | list tools, call a tool with JSON | system | |
| osquery | run SQL queries, list tables | system | osquery-go against the osqueryd extension socket |

`pkg/weavewire` defines the wire contract (ops and payloads) for presence, exec, power,
time and metrics today. The other names are reserved there, and each one gets its ops
when its module lands. A variant answers `unsupported` (`weavewire.CodeUnsupported`) for
an op its OS cannot perform, rather than imitating it.

## Layout

```
go.work                    the workspace: every module below joins it (committed)
pkg/                       github.com/weaveplatform/weaveplatform-agent-modules/pkg
  weavewire/               capability names and addresses, op kinds, payloads, stream chunks
  weaveclient/             host client: typed calls, exec sessions, auth, pluggable transport
  weavemodule/             the module runtime: one capability Service per module, parity and manifest checks
    weavemoduletest/       in-memory host and a stand-in core for testing services and hosts
  weaveagent/              dispatch and event emission under weavemodule
  weavepresence/ weaveexec/ weavepower/ weavetime/ weavemetrics/
                           OS-neutral capability services; each per-OS module supplies the backend
  weavepolicy/             exec policy and audit records
modules/                   weave-<os>-<capability>/ (next: presence, exec, power, time, metrics)
tools/                     pinned developer tools (go-test-coverage, govulncheck); not in go.work
```

A per-OS module is a `main.go` and an OS backend:

```go
modulesdk.Serve(weavemodule.New(
    weavemodule.ModuleID("linux", weavewire.Exec), // weave-linux-exec
    weaveexec.NewService(weaveexec.UnixStarter{}),
))
```

Its tests call `weavemodule.CheckParity` (it serves exactly its capability's ops) and
`weavemodule.CheckManifest` (its manifest declares the capability's address and its
one OS).

## Host side

```go
// conn is whatever reaches core directly: an HvSocket or vsock net.Conn, a Unix
// socket into a container, or weaveclient.Pipes(r, w) over a console pipe pair.
client := weaveclient.New(ctx, conn, weaveclient.Options{})
_ = client.Authenticate(ctx, channelKey)      // the machine's channel key; hello works before this
inv, _ := client.Inventory(ctx)               // → weave.presence
s, _ := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"uname", "-a"}})
```

`weaveclient.Dial` takes a `Dialer` and authenticates in the same step.

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
