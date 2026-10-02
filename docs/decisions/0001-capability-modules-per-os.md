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
  `guestweave-<linux|macos|windows>-<capability>`. Each one is its own Go module, release
  and manifest, and lists exactly one OS.
- **The channel address names the capability, not the OS.** Every variant declares the
  manifest `address` `guestweave.<capability>` (agent-core sdk v0.10.0, which also has
  `Manifest.ChannelAddress()`). Core routes by that address and refuses a second module
  that claims it. Op kinds are `guestweave.<capability>.<op>`.
- **Shared logic lives in `pkg/`.** `guestwire` holds the contract, `guestmodule` the
  runtime, and `guest<capability>` the OS-neutral service. A module contributes only its
  OS backend.
- **Parity is tested.** `guestmodule.CheckParity` asserts that a variant serves exactly
  its capability's ops. If an OS cannot perform an op, the variant registers it as
  `Unsupported`, and the host can feature-gate on that answer.
- **`guestweave.presence.hello` stays pre-auth.** agent-core allows exactly this kind
  through before the host authenticates (`hvchannel.PreAuthKind`), so it must keep that
  spelling.

## Consequences

- Hosts never branch on guest OS to reach a capability.
- A guest image installs the module set it supports. A host learns what is present from
  presence, and treats an absent capability as an unanswered address.
- With 19 capabilities across 3 OSes there are 57 modules to release. CI generates its
  matrix from `go.work`, and release-please has one component per module.
- The wire changed from the single-module contract (`guestweave/1`) to `guestweave/2`.
  Inventory moved under presence (`guestweave.presence.inventory`), and a host built for
  one cannot talk to a guest running the other.
