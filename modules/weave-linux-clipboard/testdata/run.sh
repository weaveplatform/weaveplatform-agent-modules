#!/bin/sh
# Run weave-linux-clipboard's tests against real display servers, inside the
# testdata/Dockerfile image (`make test-linux-clipboard`): a headless Sway for
# the Wayland data-control tests, and Xvfb, which the tests start themselves,
# for the X11 ones. Arguments go to `go test`.
set -eu

XDG_RUNTIME_DIR=$(mktemp -d)
export XDG_RUNTIME_DIR
WLR_BACKENDS=headless WLR_LIBINPUT_NO_DEVICES=1 WLR_RENDERER=pixman \
	sway --config /dev/null >"$XDG_RUNTIME_DIR/sway.log" 2>&1 &
sway=$!
trap 'kill "$sway" 2>/dev/null || true' EXIT

# Sway names its socket wayland-N once it accepts connections.
display=
for _ in $(seq 50); do
	for s in "$XDG_RUNTIME_DIR"/wayland-*; do
		case $s in *.lock) ;; *) [ -S "$s" ] && display=${s##*/} ;; esac
	done
	[ -n "$display" ] && break
	sleep 0.1
done
if [ -z "$display" ]; then
	echo "sway did not start:" >&2
	cat "$XDG_RUNTIME_DIR/sway.log" >&2
	exit 1
fi

export WEAVE_CLIPBOARD_TEST_WAYLAND_DISPLAY="$display"
go test "$@" ./...
