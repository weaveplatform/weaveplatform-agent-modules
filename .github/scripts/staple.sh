#!/usr/bin/env bash
# Staples a notarised package and validates the ticket, retrying both: stapler
# fetches the ticket from Apple's CloudKit ticket service, which times out now
# and then (NSURLErrorDomain -1001, stapler exit 68). A notarised package whose
# ticket has not propagated yet fails the same way. Five attempts with
# 15/30/60/120s backoff ride out both; a ticket that is still missing after
# that is a real failure.
#
#   staple.sh <pkg>
set -euo pipefail

pkg=${1:?usage: staple.sh <pkg>}
attempts=${STAPLE_ATTEMPTS:-5}
delay=${STAPLE_DELAY:-15}

try() {
	local n=1 d=$delay
	until "$@"; do
		if [ "$n" -ge "$attempts" ]; then
			echo "::error::$* failed after $attempts attempts"
			return 1
		fi
		echo "::warning::$* failed (attempt $n of $attempts); retrying in ${d}s"
		sleep "$d"
		n=$((n + 1))
		d=$((d * 2))
	done
}

try xcrun stapler staple "$pkg"
try xcrun stapler validate "$pkg"
