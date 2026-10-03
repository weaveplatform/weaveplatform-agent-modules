# sdk

The module side of the Weave agent protocol: everything a module needs to be launched,
supervised and served by `weave-agent`, and everything the weave capability modules in this
repository share. A nested Go module, tagged `sdk/vX.Y.Z`:

```
require github.com/weaveplatform/weaveplatform-agent-modules/sdk vX.Y.Z
```

It depends only on infrastructure (grpc, protobuf, x/sys, go-winio, creack/pty,
go-bindings-win32) and builds with `CGO_ENABLED=0`. It never imports agent-core; CI fails
the build if any module in this repository reaches it.

## Who owns what

This follows the Terraform split between core and the provider side.

- **agent-core owns the protocol.** `proto/` and `schema/` live in
  [weaveplatform-agent-core](https://github.com/weaveplatform/weaveplatform-agent-core),
  where the protocol is served. Core generates its own Go from them and keeps a private
  implementation under `internal/`. It never imports this sdk (its
  [ADR 0001](https://github.com/weaveplatform/weaveplatform-agent-core/blob/main/docs/decisions/0001-core-owns-its-protocol.md)).
- **This sdk is the module side.** It generates its own copy of the protocol from core's
  `proto/` at the release named in [`../.github/agent-core-version`](../.github/agent-core-version),
  and implements the module half: the handshake answer, the lifecycle server, the host
  client and the test harness. Terraform's equivalent is `terraform-plugin-go`, which carries
  its own copy of the proto at `tfprotov5/internal/tfplugin5`, with
  `terraform-plugin-framework` built on it and `terraform-plugin-testing` running the real
  `terraform` binary through `terraform-exec`, as the compat check below runs the real
  `weave-agent`. See [ADR 0002](../docs/decisions/0002-module-sdk-terraform-model.md).

The two sides agree on the wire, not on a Go package. Three things hold that agreement:

| Check | Where |
|---|---|
| `sdk/gen` is exactly what core's `proto/` generates at the pinned release | quality gate, `sdk/gen matches agent-core proto/` (`make sdk-gen`) |
| a module built on this sdk runs under the **released** `weave-agent` and core reports it running and healthy, on Linux, macOS and Windows | quality gate, `compat` (`make compat`) |
| breaking proto changes need a new package (`weave/agent/v2`) | core's `buf breaking` |

Two pieces of the wire are hand-written rather than generated, and exist on both sides: the
handshake line ([`protocol/handshake`](protocol/handshake)) and the hypervisor channel
framing ([`protocol/hvchannel`](protocol/hvchannel)). Nothing negotiates either, so a change
to one is a protocol change and must land in core as well. The compat job catches a handshake
mismatch; it cannot catch an hvchannel one, which only a host talking to a guest exercises.

## Packages

| Package | What |
|---|---|
| `modulesdk` | The module runtime: implement `Module`, call `modulesdk.Serve(m)`. Handshake, lifecycle dispatch, health, the watchdog and the `Host` client are handled for you |
| `modulesdk/testkit` | `StubCore`, which performs the core side of the handshake against a real module binary, and in-memory host services |
| `gen/go/weave/agent/v1` | Protocol 1, generated from agent-core's `proto/`. Core's operator socket (`weave/control/v1`) is deliberately absent: it is not a module contract |
| `protocol/handshake` | The environment core sets, the one stdout line a module answers with, exit code 78 for a clean refusal |
| `protocol/ipc` | Unix sockets and Windows named pipes behind one Listen/Dial seam, with per-OS peer credentials |
| `protocol/hvchannel` | The hypervisor channel's framing, envelope and Ed25519 challenge/response |
| `protocol/manifest` | Types and validation for the module manifest, the signed channel manifest and detached signatures. Verification lives in core |
| `platform` | Host info, well-known paths, session helpers |
| `config`, `werror`, `wlog` | Config document loading, error sentinels, `slog` construction |
| `weavewire` | The weave capability wire contract: capabilities, kinds, request and response payloads. The only place it is defined |
| `weavemodule`, `weavemodule/weavemoduletest` | The runtime every weave capability module is built on: one `Service` per capability, addressed as `weave.<capability>` |
| `weaveagent` | The dispatcher that routes hypervisor channel requests to handlers and orders replies |
| `weaveclient` | The host-side client for the same wire, for CLIs that drive a guest |
| `weaveclipboard`, `weavedisplay`, `weaveexec`, `weavemetrics`, `weavepolicy`, `weavepower`, `weavepresence`, `weavesession`, `weavetime` | One OS-neutral `Service` per capability; each `modules/weave-<os>-<capability>` supplies only its OS backend |

## Writing a module

Start with [`docs/writing-a-module.md`](docs/writing-a-module.md). Then copy the closest module
under [`../modules`](../modules).

## Rules

- **Breaking wire changes bump the protocol** in core: a new `weave/agent/v2` beside `v1`,
  never an edit in place. Moving `.github/agent-core-version` and running `make sdk-gen` is
  how this sdk follows.
- **`Host` is closed by default.** A new host RPC is an architecture decision in core, not a
  change here.
- Modules never import each other, never open their own sockets, never draw UI.
- Comments here are documentation for people outside this repository.
