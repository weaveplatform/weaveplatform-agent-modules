# Engineering conventions

These apply to every weaveplatform repository created from the template, with the
multi-module specifics of this repository noted.

- **Correctness loop first.** Stand up the quality gate and acceptance tests
  against real dependencies before most features exist, so every later change
  lands inside a working test loop. Here the real dependency is the released
  `weave-agent`: the `compat` job runs a module built on the sdk under it.
- **Coverage gate.** ≥95% total and ≥90% per package, merged across unit tests on
  every OS a module runs on, enforced by each module's `.testcoverage.yml`. It
  blocks merges.
- **Layout.** The public library is the nested `sdk/` module; the capability
  modules are `modules/weave-<os>-<capability>/`; `packaging/moduledeb` and
  `packaging/modulepkg` are build tools, each with its own module. Project-only packages live in `internal/`.
  Every module requires the sdk with `replace => ../../sdk`.
- **Protocol.** agent-core owns `proto/` and keeps a private implementation; `sdk/` is
  the module side; neither it nor any module imports agent-core (CI fails one).
  `sdk/gen` is generated from core's `proto/` at the tag in
  `.github/agent-core-version` by `make sdk-gen` and is never edited by hand. The
  handshake line (`sdk/protocol/handshake`) and hvchannel framing
  (`sdk/protocol/hvchannel`) are hand-written on both sides: a change to either is
  a protocol change that must land in core too. See
  `docs/decisions/0002-module-sdk-terraform-model.md`.
- **CLI.** cobra, without viper. Configuration is strict YAML that rejects unknown
  keys, with flag > environment > file precedence.
- **REST.** All HTTP APIs, including test servers that model an API, use
  [chi](https://github.com/go-chi/chi).
- **CI.** OS matrices use the `-latest` runner labels. Actions are pinned by commit
  SHA with the version in a comment; Dependabot keeps them current. Workflows
  pass actionlint with no shellcheck findings.
- **Linting.** golangci-lint with `.golangci.yml` is the only linter, and it blocks.
- **Workspace.** This repository is a Go workspace: `go.work` is committed and lists
  every module (`sdk/`, `modules/weave-<os>-<capability>/`, `packaging/moduledeb`,
  `packaging/modulepkg`).
  A new module joins `go.work`, `.github/dependabot.yml` and, when it is released,
  `release-please-config.json` and `.release-please-manifest.json` (at `0.0.0`) in
  the PR that adds it; CI fails a `go.mod` missing from `go.work`. CI also builds
  and tests every module with `GOWORK=off`, so the workspace never hides a
  requirement missing from a module's own `go.mod`.
- **Coverage per module.** Each module has its own `.testcoverage.yml`, which lists
  one profile per OS it runs on (`cover-<linux|macos|windows>.out`). Its
  exclusions are relative to the module root.
- **Capabilities.** One module per capability per OS. The channel address is
  `weave.<capability>`, and the wire contract lives only in `sdk/weavewire`.
  See `docs/decisions/0001-capability-modules-per-os.md`. Every file in a
  capability module except `doc.go` carries its OS's build tag, so the module
  builds only for that OS; CI tests, lints and vets it there alone and
  cross-compiles it for the platforms its `module.manifest.json` lists.
- **Releases.** release-please has one component per released module (`sdk`
  and each `modules/weave-<os>-<capability>`), tagged `<path>/vX.Y.Z`; it bumps a
  module's `module.manifest.json` `version` with its release. A module tag runs
  `module-release.yml`, which refuses a tag whose directory, manifest id and
  manifest version disagree, and signs and notarises a darwin module on macOS
  with the team its manifest pins (`signing.apple_team_id`) before stamping its
  digest: a release weave-agent refuses an unsigned macOS module. Nothing here publishes a version below 0.2.0: the Go
  checksum database and GHCR already hold 0.1.x.
- **Commits and PR titles** follow Conventional Commits; release-please derives
  versions and the changelog from them.

## Comments

Comment the reasoning a reader cannot recover from the code: why this and not the
obvious alternative, what a silent failure would look like, a number that must
match something elsewhere. Do not restate what the code says.
