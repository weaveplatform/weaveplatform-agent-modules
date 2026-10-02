SHELL := /bin/bash
.DEFAULT_GOAL := help

GO ?= go
GOLANGCI_LINT ?= golangci-lint
ROOT := $(CURDIR)
TOOLS := GOWORK=off $(GO) tool -modfile=$(ROOT)/tools/go.mod

# Every module in the workspace, from go.work: a module that has not joined the
# workspace is not built here, and CI fails the PR that forgot to add it.
MODULES := $(shell $(GO) work edit -json | sed -n 's/.*"DiskPath": "\.\/\(.*\)".*/\1/p')

# Coverage profiles are named for the OS that produced them, so the per-OS
# profiles CI collects merge without renaming; .testcoverage.yml lists them.
HOST_OS := $(shell $(GO) env GOOS)
COVER_OS := $(if $(filter darwin,$(HOST_OS)),macos,$(HOST_OS))

# A capability module (modules/weave-<os>-<capability>) builds only for its
# own OS. module_goos sets $$mos to that GOOS, or to nothing for a module that
# builds everywhere (pkg/).
module_goos = case $$m in modules/weave-linux-*) mos=linux;; modules/weave-macos-*) mos=darwin;; \
	modules/weave-windows-*) mos=windows;; *) mos=;; esac

# Run one recipe line in every module, stopping at the first failure. $$mos is
# the module's own GOOS, if it has one.
define each_module
	@set -e; for m in $(MODULES); do $(module_goos); echo "== $$m"; (cd $$m && $(1)); done
endef

# As each_module, skipping a capability module for another OS: its tests and
# coverage only exist on its own.
define each_host_module
	@set -e; for m in $(MODULES); do $(module_goos); \
		if [ -n "$$mos" ] && [ "$$mos" != "$(HOST_OS)" ]; then echo "== $$m (skipped: $$mos only)"; continue; fi; \
		echo "== $$m"; (cd $$m && $(1)); done
endef

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | column -t -s ':'

## modules: print the modules make and CI operate on
modules:
	@printf '%s\n' $(MODULES)

## test: every module's tests for this OS (race, shuffle) with a coverage profile for this OS
test:
	$(call each_host_module,$(GO) test -race -shuffle=on -count=1 -coverpkg=./... -coverprofile=cover-$(COVER_OS).out ./...)

## standalone: build and test every module with GOWORK=off, as a consumer would
standalone:
	$(call each_host_module,GOWORK=off $(GO) build ./... && GOWORK=off $(GO) test -count=1 ./...)

## cover: enforce each module's .testcoverage.yml over the profiles present (run make test first)
cover:
	@set -e; for m in $(MODULES); do $(module_goos); \
		if [ -n "$$mos" ] && [ "$$mos" != "$(HOST_OS)" ]; then echo "== $$m (skipped: $$mos only)"; continue; fi; \
		echo "== $$m"; ( \
		cd $$m; \
		for p in $$(sed -n 's/^profile: *//p' .testcoverage.yml | tr ',' ' '); do \
			if [ ! -s $$p ]; then echo "  $$p missing; counting it as empty (CI supplies every OS)"; echo 'mode: atomic' > $$p; fi; \
		done; \
		$(TOOLS) go-test-coverage --config=.testcoverage.yml ); done

## lint: golangci-lint over every module (a capability module as its own OS)
lint:
	$(call each_module,GOOS=$$mos GOWORK=off $(GOLANGCI_LINT) run --config $(ROOT)/.golangci.yml --new=false --fix=false ./...)

## fmt: apply the formatters configured in .golangci.yml
fmt:
	$(call each_module,GOWORK=off $(GOLANGCI_LINT) fmt --config $(ROOT)/.golangci.yml ./...)

## vet-all-os: go vet every module for linux, darwin and windows (a capability module for its own OS)
vet-all-os:
	$(call each_module,for os in $${mos:-linux darwin windows}; do echo "  $$os"; GOOS=$$os $(GO) vet ./... || exit 1; done)

## vuln: govulncheck over every module (built for this machine, run as each module's OS)
GOVULNCHECK := $(ROOT)/.bin/govulncheck
vuln:
	@mkdir -p $(ROOT)/.bin && cd $(ROOT)/tools && GOWORK=off $(GO) build -o $(GOVULNCHECK) golang.org/x/vuln/cmd/govulncheck
	$(call each_module,GOOS=$$mos GOWORK=off $(GOVULNCHECK) ./...)

## tidy: go mod tidy every module and sync the workspace
tidy:
	$(call each_module,GOWORK=off $(GO) mod tidy)
	cd tools && GOWORK=off $(GO) mod tidy
	$(GO) work sync

## clean: remove coverage profiles
clean:
	@for m in $(MODULES); do rm -f $$m/cover-*.out; done

## gate: everything CI runs, in order
gate: vet-all-os lint test standalone cover vuln

.PHONY: help modules test standalone cover lint fmt vet-all-os vuln tidy clean gate
