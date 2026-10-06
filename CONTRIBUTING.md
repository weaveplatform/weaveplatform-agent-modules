# Contributing

Thanks for considering a contribution. Please follow the
[code of conduct](CODE_OF_CONDUCT.md) in all interactions.

- **Issues.** File bugs and feature requests with the issue templates.
- **Pull requests.** Titles follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `chore:` …); release-please builds the changelog and
  the version from them, and a check enforces the format.
- **Before pushing,** run `make gate`. CI runs the same targets and the quality
  gate is the required check.

## What CI runs for a pull request

The quality gate runs only the modules a pull request needs.
`.github/scripts/select-modules.sh` compares the branch with its merge base and
applies these rules to each changed file:

| Changed | Runs |
|---|---|
| a file in `modules/<m>/` | that module |
| a file in `sdk/` | the sdk and every module |
| a file in `packaging/<p>/` | that packager |
| `go.work`, `go.work.sum`, `tools/`, `proto/`, `.golangci.yml`, `.testcoverage.yml`, `Makefile`, `.gitattributes`, `.github/workflows/`, `.github/scripts/`, `.github/golangci-lint-version`, `.github/deps-refresh.pins`, `.github/agent-core-version` | everything |
| Markdown anywhere, and anything else outside a module | no module jobs |

A selected module also selects every module whose `go.mod` replaces it with an
in-repo path, transitively, which is how the sdk selects every module. The graph
is read from the `go.mod` files, so a new module needs no change to the script.

- **Markdown** never selects a module: nothing builds, tests or lints it. This
  is why a release-please PR runs exactly the modules it releases. It edits
  their `CHANGELOG.md`, which is ignored, and their `module.manifest.json`
  version, which selects them. Its `.release-please-manifest.json` change
  selects nothing.
- **Packagers** select only themselves. No module's `go.mod` uses them, and
  no module job runs them: `module-release.yml` and `make debs`, `make pkgs` and
  `make zips` do. To try a packager change against a real module, run
  `module-release.yml` by hand with `sign_check`.
- **Everything** runs on a push to `main`, every night (so a new govulncheck
  advisory fails the gate without a code change), and on a manual run. To
  force a full run for a branch, start **go | Quality gate** from the Actions
  tab with **Run workflow** on that branch, or run
  `gh workflow run quality-gate.yml --ref <branch>`.

`Quality gate` stays the single required check. It passes when every selected
job passed and the others were skipped. The selection rules are tested by
`.github/scripts/select-modules_test.sh`.
