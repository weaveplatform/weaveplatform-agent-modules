# 0002 — The module sdk lives here; core owns the protocol

**Status:** accepted, 2026-10-03

## Context

Weave splits the agent the way Terraform splits its CLI from its providers. Terraform core
owns the plugin protocol and implements its own side of it. The provider side is a family
of libraries in their own repositories:

- `hashicorp/terraform-plugin-go` carries its own copy of the protocol, generated into
  `tfprotov5/internal/tfplugin5` (and `tfprotov6/internal/tfplugin6`), and implements the
  provider half of the wire;
- `hashicorp/terraform-plugin-framework` is what providers are written with, built on
  terraform-plugin-go;
- `hashicorp/terraform-plugin-testing` runs the real `terraform` binary against a
  provider, through `hashicorp/terraform-exec`, so acceptance tests exercise the released
  core rather than a stand-in.

Core and providers agree on the wire, not on a Go package. Dependencies point one way:
providers import the provider libraries; nothing imports core.

agent-core owns `proto/` and keeps its own implementation of the protocol
(agent-core [ADR 0001](https://github.com/weaveplatform/weaveplatform-agent-core/blob/main/docs/decisions/0001-core-owns-its-protocol.md)).

## Decision

- **`sdk/` in this repository is the module side of the protocol**:
  `github.com/weaveplatform/weaveplatform-agent-modules/sdk`, released as `sdk/vX.Y.Z`.
  It holds the module runtime (`modulesdk`, `modulesdk/testkit`), the hand-written wire
  (`protocol/{handshake,ipc,hvchannel,manifest}`), `config`, `platform`, `werror`, `wlog`,
  and everything the weave capabilities share: the wire contract (`weavewire`), the
  capability runtime (`weavemodule`), the host client (`weaveclient`) and the OS-neutral
  services (`weave<capability>`). It never imports agent-core.
- **The sdk generates its own copy of the protocol** into `sdk/gen/go/weave/agent/v1`,
  from agent-core's `proto/` at the core release named in `.github/agent-core-version`, read
  directly from git by `buf` (`make sdk-gen`). The operator socket (`weave/control/v1`) is
  core's alone and is not generated here. CI regenerates and fails on any difference.
- **Every module builds on the sdk alone.** A module requires the sdk through
  `replace => ../../sdk`, so it always builds against the sdk at its own commit: in the
  workspace, with `GOWORK=off`, and in the release pipeline, which checks out the tag. CI
  fails the sdk, or any module as its own OS, if its dependencies reach agent-core.
- **Compatibility is tested against the released core**, the terraform-plugin-testing way:
  the quality gate downloads the `weave-agent` release named in `.github/agent-core-version`
  and runs a module built from this sdk under it on Linux, macOS and Windows, until core's
  own `weavectl` reports it running and healthy. The test verifies the module through a
  channel manifest it signs with core's `weavemanifest` (the offline-install path, the same
  on every OS), so it needs no platform code-signing identity.
- **Host inventory is the presence capability.** `weave-<os>-presence` serves the host's
  identity, hardware and network inventory (`weavewire.InventoryResponse`); a periodic push,
  if a host wants one, is an event `weavepresence` emits.
- **Modules publish from here.** Each module releases on its own tag, and this
  repository's `module-release.yml` builds it, pushes it to GHCR and offers it to
  weaveplatform-release-channels for promotion. Core has no module pipeline, as Terraform core has
  no provider release workflow.

## Consequences

- A module-side change ships in one pull request, here, with the modules it serves, and
  needs no core release.
- Moving to a new core release is deliberate: bump `.github/agent-core-version`, run
  `make sdk-gen`, and the gate's compat job runs the new core.
- Two hand-written pieces of the wire exist on both sides: the handshake line and the
  hypervisor channel framing. The generated code cannot drift; these can, and a change to
  either must land in core as well. The compat job catches a handshake mismatch, not an
  hvchannel one.
- A parser fix in the sdk's implementation does not fix core's, and the reverse.
- A module release carries the sdk at that commit; the sdk's `sdk/vX.Y.Z` tags are for
  third-party module authors, who require it like any Go module.

## References

- agent-core, core's side: [ADR 0001](https://github.com/weaveplatform/weaveplatform-agent-core/blob/main/docs/decisions/0001-core-owns-its-protocol.md), `proto/`, `internal/protocol`, `internal/gen`
- `hashicorp/terraform-plugin-go`: its own protocol copy at `tfprotov5/internal/tfplugin5`
- `hashicorp/terraform-plugin-framework`: the provider library built on terraform-plugin-go
- `hashicorp/terraform-plugin-testing`: acceptance tests that run the real `terraform` binary via `hashicorp/terraform-exec`
