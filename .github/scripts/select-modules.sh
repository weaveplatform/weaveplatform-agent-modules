#!/usr/bin/env bash
# Decides which workspace modules the quality gate runs for a change.
#
#   select-modules.sh [--all] < changed-paths
#
# Run from the repository root with the changed paths (relative to the root,
# one per line) on stdin, or with --all for a full run. Prints GITHUB_OUTPUT
# lines:
#
#   all       true when every module was selected (--all or a shared input)
#   any       true when at least one module was selected
#   sdk       true when sdk/ was selected (its own extra jobs run then)
#   selected  the selected module directories, as a JSON array
#   modules   matrix of {dir, slug, goos}, one per module
#   units     matrix of {dir, slug, os, runner}, one per module per OS it runs on
#
# The rules, in order, for each changed path:
#
#   1. Markdown (*.md) anywhere selects nothing: no build, test, lint or
#      coverage input is Markdown, and a release-please PR's CHANGELOG.md edits
#      would otherwise select every module it releases twice over.
#   2. A shared input selects everything: the workspace, the tool versions, the
#      lint and coverage config, the Makefile, the workflows and their scripts.
#   3. A path inside a module selects that module (the longest go.work entry
#      that contains it).
#   4. Anything else (repository docs, release-please's root manifest and
#      config, issue templates) selects nothing.
#
# Then every module whose go.mod replaces a selected module with an in-repo
# path (../../sdk) is selected too, transitively. The graph comes from the
# go.mod files, so a new module or a new in-repo dependency needs no edit here.
set -euo pipefail

all=false
case "${1:-}" in
  --all) all=true ;;
  '') ;;
  *) echo "usage: $0 [--all] < changed-paths" >&2; exit 2 ;;
esac

mods=$(go work edit -json | jq -c '[.Use[].DiskPath | ltrimstr("./")] | sort')

# One {dir, deps} per module: deps are the in-repo directories its go.mod
# replaces a requirement with, made relative to the repository root.
graph=$(
  for d in $(jq -r '.[]' <<<"$mods"); do
    (cd "$d" && go mod edit -json) | jq -c --arg d "$d" '
      def norm: reduce (split("/")[]) as $s ([];
        if $s == "" or $s == "." then . elif $s == ".." then .[:-1] else . + [$s] end) | join("/");
      {dir: $d, deps: [.Replace[]? | .New.Path
        | select(startswith("./") or startswith("../")) | "\($d)/\(.)" | norm]}'
  done | jq -sc .
)

files=$(jq -Rsc 'split("\n") | map(sub("\r$"; "")) | map(select(length > 0))')

result=$(jq -nc --argjson graph "$graph" --argjson files "$files" --argjson all "$all" '
  def shared:
    IN("go.work", "go.work.sum", "Makefile", ".golangci.yml", ".golangci.yaml",
       ".testcoverage.yml", ".gitattributes", ".github/golangci-lint-version",
       ".github/deps-refresh.pins", ".github/agent-core-version")
    or (startswith("tools/") or startswith("proto/")
        or startswith(".github/workflows/") or startswith(".github/scripts/"));
  ($graph | map(.dir)) as $dirs
  | ($files | map(select(endswith(".md") | not))) as $build
  | ($build | map(select(shared))) as $triggers
  | ($all or ($triggers | length > 0)) as $everything
  | (if $everything then $dirs else
      [$build[] as $f | [$dirs[] | select(. as $d | $f | startswith($d + "/"))]
        | max_by(length) // empty] | unique
    end) as $direct
  # Fixpoint: add every module that replaces one already selected.
  | ($direct | until(. as $s | [$graph[] | select(any(.deps[]; IN($s[]))) | .dir] - $s | length == 0;
      . as $s | ($s + [$graph[] | select(any(.deps[]; IN($s[]))) | .dir]) | unique)) as $selected
  | {all: $everything, triggers: $triggers, direct: $direct, selected: $selected}')

jq -r '
  if .all then "every module (\(if (.triggers | length) > 0 then "shared input: \(.triggers | join(", "))" else "full run" end))"
  elif (.selected | length) == 0 then "no module: nothing changed that a module builds from"
  else "changed: \(.direct | join(", "))", "selected: \(.selected | join(", "))" end' <<<"$result" >&2

selected=$(jq -c .selected <<<"$result")

# A capability module (modules/weave-<os>-<capability>) runs on its own OS
# only; any other module (sdk/, the packaging tools) runs on all three.
modules=$(jq -c '{include: map(. as $d | {dir: $d, slug: ($d | gsub("/"; "-")),
  goos: ((($d | capture("^modules/weave-(?<os>linux|macos|windows)-")? | .os) // "")
    | {linux: "linux", macos: "darwin", windows: "windows", "": ""}[.])})}' <<<"$selected")
units=$(jq -c '{include: [.[] as $d
  | (($d | capture("^modules/weave-(?<os>linux|macos|windows)-")? | .os) // null) as $only
  | {linux: "ubuntu-latest", macos: "macos-latest", windows: "windows-latest"} | to_entries[]
  | select($only == null or .key == $only)
  | {dir: $d, slug: ($d | gsub("/"; "-")), os: .key, runner: .value}]}' <<<"$selected")

echo "$(jq '.include | length' <<<"$modules") modules, $(jq '.include | length' <<<"$units") module-OS units" >&2

echo "all=$(jq -r .all <<<"$result")"
echo "any=$(jq -r '.selected | length > 0' <<<"$result")"
echo "sdk=$(jq -r '.selected | index("sdk") != null' <<<"$result")"
echo "selected=$selected"
echo "modules=$modules"
echo "units=$units"
