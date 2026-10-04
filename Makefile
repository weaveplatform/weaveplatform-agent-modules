SHELL := /bin/bash
.DEFAULT_GOAL := help

GO ?= go
GOLANGCI_LINT ?= golangci-lint
ROOT := $(CURDIR)
TOOLS := GOWORK=off $(GO) tool -modfile=$(ROOT)/tools/go.mod

# Every module in the workspace, from go.work: a module that has not joined the
# workspace is not built here, and CI fails the PR that forgot to add it.
WORKSPACE := $(shell $(GO) work edit -json | sed -n 's/.*"DiskPath": "\.\/\(.*\)".*/\1/p')

# Coverage profiles are named for the OS that produced them, so the per-OS
# profiles CI collects merge without renaming; .testcoverage.yml lists them.
HOST_OS := $(shell $(GO) env GOOS)
COVER_OS := $(if $(filter darwin,$(HOST_OS)),macos,$(HOST_OS))

# A capability module (modules/weave-<os>-<capability>) builds only for its
# own OS. module_goos sets $$mos to that GOOS, or to nothing for a module that
# builds everywhere (sdk/).
module_goos = case $$m in modules/weave-linux-*) mos=linux;; modules/weave-macos-*) mos=darwin;; \
	modules/weave-windows-*) mos=windows;; *) mos=;; esac

# Run one recipe line in every module, stopping at the first failure. $$mos is
# the module's own GOOS, if it has one.
define each_module
	@set -e; for m in $(WORKSPACE); do $(module_goos); echo "== $$m"; (cd $$m && $(1)); done
endef

# As each_module, skipping a capability module for another OS: its tests and
# coverage only exist on its own.
define each_host_module
	@set -e; for m in $(WORKSPACE); do $(module_goos); \
		if [ -n "$$mos" ] && [ "$$mos" != "$(HOST_OS)" ]; then echo "== $$m (skipped: $$mos only)"; continue; fi; \
		echo "== $$m"; (cd $$m && $(1)); done
endef

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | column -t -s ':'

## modules: print the modules make and CI operate on
modules:
	@printf '%s\n' $(WORKSPACE)

## test: every module's tests for this OS (race, shuffle) with a coverage profile for this OS
test:
	$(call each_host_module,$(GO) test -race -shuffle=on -count=1 -coverpkg=./... -coverprofile=cover-$(COVER_OS).out ./...)

## standalone: build and test every module with GOWORK=off, as a consumer would
standalone:
	$(call each_host_module,GOWORK=off $(GO) build ./... && GOWORK=off $(GO) test -count=1 ./...)

## cover: enforce each module's .testcoverage.yml over the profiles present (run make test first)
cover:
	@set -e; for m in $(WORKSPACE); do $(module_goos); \
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

## vuln: govulncheck over every module, as each OS it builds for (the tool is built for this machine)
GOVULNCHECK := $(ROOT)/.bin/govulncheck
vuln:
	@mkdir -p $(ROOT)/.bin && cd $(ROOT)/tools && GOWORK=off $(GO) build -o $(GOVULNCHECK) golang.org/x/vuln/cmd/govulncheck
	$(call each_module,for os in $${mos:-linux darwin windows}; do echo "  $$os"; GOOS=$$os GOWORK=off $(GOVULNCHECK) ./... || exit 1; done)

## tidy: go mod tidy every module and sync the workspace
tidy:
	$(call each_module,GOWORK=off $(GO) mod tidy)
	cd tools && GOWORK=off $(GO) mod tidy
	$(GO) work sync

# The sdk's generated protocol comes from agent-core's proto/ at the release
# named in .github/agent-core-version, read straight from git by buf. Only
# weave/agent/v1: control/v1 is core's operator socket, not a module contract.
AGENT_CORE_VERSION := $(shell tr -d '[:space:]' < $(ROOT)/.github/agent-core-version)
AGENT_CORE_PROTO := https://github.com/weaveplatform/weaveplatform-agent-core.git\#tag=$(AGENT_CORE_VERSION),subdir=proto

## sdk-gen: regenerate sdk/gen/go from agent-core's proto/ at .github/agent-core-version
sdk-gen:
	cd sdk && buf generate --path weave/agent/v1 '$(AGENT_CORE_PROTO)'

## compat: run the sdk's compat fixture under a released weave-agent (AGENT_DIR=<extracted release archive>)
compat:
	@test -n "$(AGENT_DIR)" || { echo "set AGENT_DIR to a directory holding weave-agent, weavectl and weavemanifest"; exit 1; }
	cd sdk && WEAVE_AGENT_DIR='$(AGENT_DIR)' GOWORK=off $(GO) test -count=1 -run TestUnderReleasedAgent -v ./internal/compatfixture

# Local bring-up: Linux module packages for an apt repository beside core's
# weave-agent package (core's packaging/apt). Each module is built the way
# module-release.yml builds it, then packaged by packaging/moduledeb.
ARCH ?= $(shell $(GO) env GOARCH)
MODULES ?= $(filter weave-linux-%,$(notdir $(WORKSPACE)))
DIST := $(ROOT)/dist
MODULEDEB := $(ROOT)/.bin/moduledeb

## debs: Linux module .debs in dist/ (ARCH=amd64|arm64, MODULES="weave-linux-presence ..."; default all)
debs:
	@mkdir -p $(ROOT)/.bin $(DIST)/.build
	cd packaging/moduledeb && GOWORK=off $(GO) build -o $(MODULEDEB) .
	@set -e; for id in $(MODULES); do \
		case $$id in weave-linux-*) ;; *) echo "$$id is not a Linux module"; exit 1;; esac; \
		dir=$(ROOT)/modules/$$id; \
		[ -f $$dir/module.manifest.json ] || { echo "no module $$id under modules/"; exit 1; }; \
		echo "== $$id linux/$(ARCH)"; \
		(cd $$dir && CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) GOWORK=off \
			$(GO) build -trimpath -o $(DIST)/.build/$$id-linux-$(ARCH) .); \
		$(MODULEDEB) -binary $(DIST)/.build/$$id-linux-$(ARCH) \
			-manifest $$dir/module.manifest.json -arch $(ARCH) -out $(DIST); \
	done

# Local bring-up on macOS: module installer packages, the counterpart of debs,
# for a guest running core's weave-agent package (agent-core
# docs/macos-package.md). Built the way module-release.yml builds a module, then
# packaged by packaging/modulepkg with pkgbuild, so this runs on macOS only.
# Core verifies a module's code signature before it launches it; the binary is
# packaged as built, so sign it first wherever core requires one.
PKG_ARCH ?= arm64
PKG_MODULES ?= $(filter weave-macos-%,$(notdir $(WORKSPACE)))
MODULEPKG := $(ROOT)/.bin/modulepkg

## pkgs: macOS module .pkgs in dist/ (MODULES="weave-macos-presence ..."; default all; PKG_ARCH=arm64)
pkgs:
	@mkdir -p $(ROOT)/.bin $(DIST)/.build
	cd packaging/modulepkg && GOWORK=off $(GO) build -o $(MODULEPKG) .
	@set -e; for id in $(if $(filter command line environment,$(origin MODULES)),$(MODULES),$(PKG_MODULES)); do \
		case $$id in weave-macos-*) ;; *) echo "$$id is not a macOS module"; exit 1;; esac; \
		dir=$(ROOT)/modules/$$id; \
		[ -f $$dir/module.manifest.json ] || { echo "no module $$id under modules/"; exit 1; }; \
		echo "== $$id darwin/$(PKG_ARCH)"; \
		(cd $$dir && CGO_ENABLED=0 GOOS=darwin GOARCH=$(PKG_ARCH) GOWORK=off \
			$(GO) build -trimpath -o $(DIST)/.build/$$id-darwin-$(PKG_ARCH) .); \
		$(MODULEPKG) -binary $(DIST)/.build/$$id-darwin-$(PKG_ARCH) \
			-manifest $$dir/module.manifest.json -arch $(PKG_ARCH) -out $(DIST); \
	done

## clean: remove coverage profiles and dist/
clean:
	@for m in $(WORKSPACE); do rm -f $$m/cover-*.out; done
	rm -rf $(DIST)

## gate: everything CI runs, in order
gate: vet-all-os lint test standalone cover vuln

.PHONY: help modules test standalone cover lint fmt vet-all-os vuln tidy sdk-gen compat debs pkgs clean gate
