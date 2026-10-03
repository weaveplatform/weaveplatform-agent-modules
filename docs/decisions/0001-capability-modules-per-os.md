# 0001 — One module per capability per OS, addressed by capability

**Status:** accepted, 2026-10-02

## Context

A machine under weave needs a set of capabilities: presence, exec, power, time, metrics,
clipboard, session, display and more. Each needs different OS code, a different privilege
and a different session: clipboard and display need the user at the physical console;
power and exec need system. A single module per OS serving every capability would release
every capability whenever one changes, let a crash in one take down the rest, and run all
of them with the privilege and session of the most demanding one.

The host, meanwhile, should not care which OS is behind a capability: running a process is
the same request on Linux, macOS and Windows.

## Decision

- **One module per capability per OS**, with the id
  `weave-<linux|macos|windows>-<capability>`. Each is its own Go module, release and
  manifest, and lists exactly one OS.
- **The channel address names the capability, not the OS.** Every variant declares the
  manifest `address` `weave.<capability>` (`Manifest.ChannelAddress()` in
  `sdk/protocol/manifest`). Core routes by that address and refuses a second module that
  claims it. Op kinds are `weave.<capability>.<op>`.
- **Shared logic lives in the sdk** (ADR 0002). `weavewire` holds the contract,
  `weavemodule` the runtime, `weaveclient` the host client, and `weave<capability>` the
  OS-neutral service. A module contributes only its OS backend.
- **Parity is tested.** `weavemodule.CheckParity` asserts that a variant serves exactly its
  capability's ops. If an OS cannot perform an op, the variant registers it as
  `Unsupported`, and the host can feature-gate on that answer.
- **Names are product-neutral.** Core and these modules serve every weave product that
  manages an OS — on a device, in a local or hyperscaler VM, in a container — so nothing on
  the wire or in a module id names one product: `weave-<os>-<capability>`,
  `weave.<capability>`, protocol `weave/1`, packages `weave*`.
- **Driven over the host channel.** The host directly outside the machine — a hypervisor
  for a VM, a container runtime for a container — drives every capability over core's host
  channel, so a module requires that channel to launch.
- **`weave.presence.hello` is pre-auth.** Core lets exactly this kind through before the
  host authenticates (`hvchannel.PreAuthKind`), so a host can tell "wrong key" from "no
  agent here". The sdk and core must agree on it.

## Consequences

- Hosts never branch on the target OS to reach a capability.
- An image installs the module set it supports. A host learns what is present from
  presence, and treats an absent capability as an unanswered address.
- Every capability multiplies into three modules to release. CI generates its matrix from
  `go.work`, and release-please has one component per module.
- A capability's privilege and session are declared per module, so each gets the
  least-privileged placement that works for it.
