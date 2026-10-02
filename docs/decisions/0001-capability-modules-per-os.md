# 0001 — One module per capability per OS, addressed by capability

**Status:** accepted, 2026-10-02

## Context

guestweave-agent shipped a single `guestweave` module per guest OS that served every
feature. Adding a feature meant releasing all three builds together, a crash in one
feature took the others down with it, and every feature ran with the privilege and
session of the most demanding one. Clipboard and display need a per-user console
session; power and exec need system.

## Decision

- **One agent-core module per capability per OS**, with the id
  `weave-<linux|macos|windows>-<capability>`. Each one is its own Go module, release
  and manifest, and lists exactly one OS.
- **The channel address names the capability, not the OS.** Every variant declares the
  manifest `address` `weave.<capability>` (agent-core sdk v0.10.0, which also has
  `Manifest.ChannelAddress()`). Core routes by that address and refuses a second module
  that claims it. Op kinds are `weave.<capability>.<op>`.
- **Shared logic lives in `pkg/`.** `weavewire` holds the contract, `weavemodule` the
  runtime, `weaveclient` the host client, and `weave<capability>` the OS-neutral service. A module contributes only its
  OS backend.
- **Parity is tested.** `weavemodule.CheckParity` asserts that a variant serves exactly
  its capability's ops. If an OS cannot perform an op, the variant registers it as
  `Unsupported`, and the host can feature-gate on that answer.
- **Names are product-neutral.** Core and these modules serve every weave product
  that manages an OS — on a device, in a local or hyperscaler VM, in a container — so
  nothing on the wire or in a module id names one product: `weave-<os>-<capability>`,
  `weave.<capability>`, protocol `weave/1`, packages `weave*`.
- **Driven over the host channel.** The host directly outside the machine — a
  hypervisor for a VM, a container runtime for a container — drives every capability
  over core's host channel, so a module requires that channel to launch.
- **`weave.presence.hello` is pre-auth.** agent-core allows exactly this kind through
  before the host authenticates (`hvchannel.PreAuthKind`), so the two must agree.

## Consequences

- Hosts never branch on the target OS to reach a capability.
- An image installs the module set it supports. A host learns what is present from
  presence, and treats an absent capability as an unanswered address.
- With 19 capabilities across 3 OSes there are 57 modules to release. CI generates its
  matrix from `go.work`, and release-please has one component per module.
- The wire is not compatible with the single-module contract it replaces
  (`guestweave/1`): inventory moved under presence (`weave.presence.inventory`), and a
  host built for one cannot talk to a machine running the other.
