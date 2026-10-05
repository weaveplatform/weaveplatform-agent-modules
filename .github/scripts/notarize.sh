#!/usr/bin/env bash
# notarize.sh <file>: submit a zip or pkg to Apple's notary service, wait for
# the verdict, print the notary log, and fail unless the verdict is Accepted.
#
# The App Store Connect API key comes from the environment: NOTARY_KEY (the
# .p8 path), NOTARY_KEY_ID and NOTARY_ISSUER. Each submission's id and status
# go to the job summary, so a release records what Apple accepted.
#
# The verdict is read from notarytool's JSON rather than inferred from its exit
# status, so a rejection still reports its submission id and fetches its log.
set -euo pipefail

file=${1:?usage: notarize.sh <file>}
: "${NOTARY_KEY:?}" "${NOTARY_KEY_ID:?}" "${NOTARY_ISSUER:?}"
auth=(--key "$NOTARY_KEY" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER")
name=$(basename "$file")

result=$(xcrun notarytool submit "$file" "${auth[@]}" --wait --timeout 30m --output-format json) || true
id=$(jq -r '.id // empty' <<<"$result" 2>/dev/null || true)
status=$(jq -r '.status // empty' <<<"$result" 2>/dev/null || true)
if [ -z "$id" ]; then
  echo "::error::notarytool returned no submission id for $name"
  printf '%s\n' "$result" >&2
  exit 1
fi
echo "notarisation of $name: submission $id, status ${status:-unknown}"

# The log names every issue, including warnings on an accepted submission.
log="${RUNNER_TEMP:-/tmp}/notary-${id}.json"
if xcrun notarytool log "$id" "${auth[@]}" "$log" >/dev/null; then
  cat "$log"
else
  echo "::warning::could not fetch the notary log for submission $id"
fi

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  echo "| \`$name\` | \`$id\` | ${status:-unknown} |" >> "$GITHUB_STEP_SUMMARY"
fi
if [ "$status" != Accepted ]; then
  echo "::error::Apple did not accept $name (submission $id, status ${status:-unknown})"
  exit 1
fi
