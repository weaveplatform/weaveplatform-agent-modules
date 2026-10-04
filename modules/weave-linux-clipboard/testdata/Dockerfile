# The display servers weave-linux-clipboard's tests run against, for
# `make test-linux-clipboard`: Xvfb (which the tests start themselves), xclip
# and wl-clipboard as other applications, and Sway, a wlroots compositor with
# wlr-data-control, run headless. The tests run as an ordinary user, as the
# module does in a console session.
ARG GO_VERSION
FROM golang:${GO_VERSION}
RUN apt-get update && \
	apt-get install -y --no-install-recommends xvfb xclip wl-clipboard sway && \
	rm -rf /var/lib/apt/lists/*
RUN useradd --create-home --uid 1000 dev
USER dev
