#!/usr/bin/env bash
# Tests for select-modules.sh: the real workspace for the cases that matter in
# practice, and a small fixture workspace for the graph rules (transitive
# in-repo replaces, a module that is nobody's dependency, nested paths).
#
#   .github/scripts/select-modules_test.sh    (from the repository root)
set -euo pipefail

root=$(git rev-parse --show-toplevel)
script="$root/.github/scripts/select-modules.sh"
failures=0
cases=0

# expect <name> <want-all> <want-selected-json> [changed paths...]
# Runs from the current directory. An empty path list means --all.
expect() {
  local name=$1 want_all=$2 want_sel=$3 out got_all got_sel
  shift 3
  cases=$((cases + 1))
  if [ $# -eq 0 ]; then
    out=$("$script" --all </dev/null 2>/dev/null)
  else
    out=$(printf '%s\n' "$@" | "$script" 2>/dev/null)
  fi
  got_all=$(sed -n 's/^all=//p' <<<"$out")
  got_sel=$(sed -n 's/^selected=//p' <<<"$out")
  if [ "$got_all" != "$want_all" ] || [ "$(jq -c 'sort' <<<"$got_sel")" != "$(jq -c 'sort' <<<"$want_sel")" ]; then
    echo "FAIL $name"
    echo "  want all=$want_all selected=$want_sel"
    echo "  got  all=$got_all selected=$got_sel"
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

# Every key must be present and the matrices consistent with the selection.
check_outputs() { # <name> [changed paths...]
  local name=$1 out
  shift
  cases=$((cases + 1))
  out=$(printf '%s\n' "$@" | "$script" 2>/dev/null)
  if ! jq -en --argjson sel "$(sed -n 's/^selected=//p' <<<"$out")" \
      --argjson mods "$(sed -n 's/^modules=//p' <<<"$out")" \
      --argjson units "$(sed -n 's/^units=//p' <<<"$out")" \
      --arg any "$(sed -n 's/^any=//p' <<<"$out")" \
      --arg sdk "$(sed -n 's/^sdk=//p' <<<"$out")" '
      ($mods.include | map(.dir)) == $sel
      and (($units.include | map(.dir) | unique) == ($sel | unique))
      and ($any == (($sel | length) > 0 | tostring))
      and ($sdk == (($sel | index("sdk")) != null | tostring))' >/dev/null; then
    echo "FAIL $name: outputs disagree"; echo "$out"
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

# --- the real workspace -----------------------------------------------------
cd "$root"
every=$(go work edit -json | jq -c '[.Use[].DiskPath | ltrimstr("./")]')
modules_only=$(jq -c 'map(select(startswith("modules/")))' <<<"$every")

expect 'one module' false '["modules/weave-windows-clipboard"]' \
  modules/weave-windows-clipboard/clipboard_windows.go
expect 'one module, file in a subpackage' false '["modules/weave-linux-exec"]' \
  modules/weave-linux-exec/internal/x/y.go
expect 'module coverage config' false '["modules/weave-macos-time"]' \
  modules/weave-macos-time/.testcoverage.yml
expect 'sdk selects sdk and every module' false "$(jq -c '. + ["sdk"]' <<<"$modules_only")" \
  sdk/plugin.go
expect 'packaging tool alone' false '["packaging/modulezip"]' \
  packaging/modulezip/main.go
expect 'markdown only' false '[]' \
  README.md docs/clipboard.md CONTRIBUTING.md modules/weave-linux-time/README.md sdk/CHANGELOG.md
expect 'files that feed no build' false '[]' \
  LICENSE .gitignore .github/CODEOWNERS .github/dependabot.yml .release-please-manifest.json release-please-config.json
expect 'release-please PR releases two modules' false '["modules/weave-linux-time","modules/weave-windows-exec"]' \
  .release-please-manifest.json \
  modules/weave-linux-time/CHANGELOG.md modules/weave-linux-time/module.manifest.json \
  modules/weave-windows-exec/CHANGELOG.md modules/weave-windows-exec/module.manifest.json
for f in .github/workflows/quality-gate.yml .github/scripts/select-modules.sh go.work go.work.sum \
  tools/go.mod .golangci.yml .testcoverage.yml Makefile .github/golangci-lint-version \
  .github/deps-refresh.pins .github/agent-core-version .gitattributes proto/x.proto; do
  expect "shared input $f" true "$every" "$f" README.md
done
expect 'full run' true "$every"
expect 'a path under modules/ outside every go.work module' false '[]' modules/weave-gone/main.go
check_outputs 'outputs agree: one module' modules/weave-windows-clipboard/x.go
check_outputs 'outputs agree: sdk' sdk/x.go
check_outputs 'outputs agree: nothing' README.md

# --- a fixture workspace ----------------------------------------------------
# base <- lib <- app, base <- lib-ext (a nested module, not under lib), and
# solo, which nothing depends on.
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
cd "$fixture"
mkmod() { # <dir> <module path> [replace targets...]
  local dir=$1 path=$2
  shift 2
  mkdir -p "$dir"
  { echo "module example.com/$path"; echo; echo "go 1.24"
    for r in "$@"; do echo "replace example.com/${r##*/} => $r"; done; } >"$dir/go.mod"
}
mkmod base base
mkmod pkgs/lib lib ../../base
mkmod pkgs/lib/ext lib-ext ../../../base
mkmod apps/app app ../../pkgs/lib
mkmod apps/solo solo
printf 'go 1.24\n\nuse (\n\t./apps/app\n\t./apps/solo\n\t./base\n\t./pkgs/lib\n\t./pkgs/lib/ext\n)\n' >go.work

expect 'fixture: leaf' false '["apps/app"]' apps/app/main.go
expect 'fixture: transitive' false '["apps/app","base","pkgs/lib","pkgs/lib/ext"]' base/base.go
expect 'fixture: middle' false '["apps/app","pkgs/lib"]' pkgs/lib/lib.go
expect 'fixture: nested module owns its own files' false '["pkgs/lib/ext"]' pkgs/lib/ext/ext.go
expect 'fixture: unrelated' false '["apps/solo"]' apps/solo/x.go
expect 'fixture: path outside every module' false '[]' apps/README.txt
expect 'fixture: go.work' true '["apps/app","apps/solo","base","pkgs/lib","pkgs/lib/ext"]' go.work

echo "$cases cases, $failures failed"
[ "$failures" -eq 0 ]
