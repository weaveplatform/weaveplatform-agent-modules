# Engineering conventions

These apply to every weaveplatform repository created from the template, with the
multi-module specifics of this repository noted.

- **Correctness loop first.** Stand up the quality gate and acceptance tests
  against real dependencies (Docker via testcontainers) before most features
  exist, so every later change lands inside a working test loop.
- **Coverage gate.** ≥95% total and ≥90% per package, merged across unit tests on
  every OS and acceptance runs, enforced by each module's `.testcoverage.yml`. It
  blocks merges.
- **Layout.** Public packages in `pkg/`, project-only packages in `internal/`,
  entry points in `cmd/`.
- **CLI.** cobra, without viper. Configuration is strict YAML that rejects unknown
  keys, with flag > environment > file precedence.
- **REST.** All HTTP APIs, including test servers that model an API, use
  [chi](https://github.com/go-chi/chi).
- **CI.** OS matrices use the `-latest` runner labels. Actions are pinned by commit
  SHA with the version in a comment; Dependabot keeps them current.
- **Linting.** golangci-lint with `.golangci.yml` is the only linter, and it blocks.
- **Workspace.** This repository is a Go workspace: `go.work` is committed and lists
  every module (`pkg/`, `modules/weave-<os>-<capability>/`, `cmd/*`). A new
  module joins `go.work`, `release-please-config.json` and `.github/dependabot.yml`
  in the PR that adds it; CI fails a `go.mod` missing from `go.work`. CI also
  builds and tests every module with `GOWORK=off`, so the workspace never hides
  a requirement missing from a module's own `go.mod`.
- **Coverage per module.** Each module has its own `.testcoverage.yml`, which lists
  one profile per OS it runs on (`cover-<linux|macos|windows>.out`). Its
  exclusions are relative to the module root.
- **Capabilities.** One module per capability per OS. The channel address is
  `weave.<capability>`, and the wire contract lives only in `pkg/weavewire`.
  See `docs/decisions/0001-capability-modules-per-os.md`.
- **Commits and PR titles** follow Conventional Commits; release-please derives
  versions and the changelog from them.

## Comments

Comment the reasoning a reader cannot recover from the code: why this and not the
obvious alternative, what a silent failure would look like, a number that must
match something elsewhere. Do not restate what the code says.
