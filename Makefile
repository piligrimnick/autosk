# autosk — Makefile
#
# The Go binary (CLI + lazy TUI) is a pure JSON-RPC client of autoskd (proto-v2)
# and builds with plain `go build`, CGO-free. autoskd is the Bun/TypeScript
# daemon under daemon/; it is compiled to a standalone binary with
# `bun build --compile` (it embeds the Bun runtime, so no global bun is needed
# at runtime).

GO       ?= go
BUN      ?= bun
BIN_DIR  := bin
BIN_NAME := autosk
BIN      := $(BIN_DIR)/$(BIN_NAME)
PKG      := ./cmd/autosk

# autoskd — the Bun daemon that owns .autosk/ (tasks/sessions/extensions on
# disk; no database). The CLI/lazy front ends are pure RPC clients, so the
# cmd/autosk verb tests need a live daemon; they locate it via $AUTOSKD_BIN (the
# connector's first lookup). Compiled with `bun build --compile`.
DAEMON_DIR          := $(CURDIR)/daemon
DAEMON_ENTRY        := $(DAEMON_DIR)/core/src/index.ts
AUTOSKD_BIN         := $(BIN_DIR)/autoskd
# The daemon-bundled extensions (the feature-dev reference workflow + its
# pi-agent roles) are compiled into a self-contained tree under $(BIN_DIR)/
# extensions by scripts/bundle-extensions.sh (each extension's @autosk/* deps
# are inlined, because a `bun build --compile` binary cannot resolve an on-disk
# extension's bare imports). The test/install targets point
# $AUTOSK_BUNDLED_EXTENSIONS at that built tree.
BUNDLED_EXTENSIONS  := $(BIN_DIR)/extensions

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS  := -X 'autosk/internal/buildinfo.Version=$(VERSION)' \
            -X 'autosk/internal/buildinfo.Commit=$(COMMIT)'

# Install destination resolution (matches `go install`):
#   1. $GOBIN if set
#   2. $GOPATH/bin (first entry of GOPATH)
#   3. $HOME/go/bin
GOBIN_DIR := $(shell $(GO) env GOBIN)
ifeq ($(strip $(GOBIN_DIR)),)
GOBIN_DIR := $(firstword $(subst :, ,$(shell $(GO) env GOPATH)))/bin
endif

# Where `make install` drops the bundled extensions: <gobin>/../libexec/autosk/
# extensions, which the installed autoskd discovers relative to its own path
# (defaultBundledDir(), no env needed). Defined after GOBIN_DIR so the abspath
# resolves against the real install root, not "/".
LIBEXEC_EXTENSIONS  := $(abspath $(GOBIN_DIR)/../libexec/autosk/extensions)

.PHONY: all build build-autoskd install install-autoskd uninstall test test-short lint \
        clean distclean tidy fmt vet help

all: build

## build: compile bin/autosk (CGO-free; pure JSON-RPC client of autoskd)
build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## build-autoskd: compile the Bun daemon into bin/autoskd + bundle its extensions
build-autoskd:
	@mkdir -p $(BIN_DIR)
	cd $(DAEMON_DIR) && $(BUN) install --frozen-lockfile >/dev/null 2>&1 || (cd $(DAEMON_DIR) && $(BUN) install)
	cd $(DAEMON_DIR) && $(BUN) build --compile $(DAEMON_ENTRY) --outfile $(CURDIR)/$(AUTOSKD_BIN)
	BUN=$(BUN) scripts/bundle-extensions.sh $(CURDIR)/$(BUNDLED_EXTENSIONS)

## install: install autosk + autoskd into $$GOBIN (or $$GOPATH/bin)
install: install-autoskd
	$(GO) install -ldflags "$(LDFLAGS)" $(PKG)
	@echo "installed: $(GOBIN_DIR)/$(BIN_NAME)"

## install-autoskd: compile the Bun daemon and install it (+ bundled extensions)
install-autoskd: build-autoskd
	@mkdir -p "$(GOBIN_DIR)"
	@install -m 0755 "$(CURDIR)/$(AUTOSKD_BIN)" "$(GOBIN_DIR)/autoskd"
	@echo "installed: $(GOBIN_DIR)/autoskd"
	@rm -rf "$(LIBEXEC_EXTENSIONS)"
	@mkdir -p "$(LIBEXEC_EXTENSIONS)"
	@cp -R "$(CURDIR)/$(BUNDLED_EXTENSIONS)/." "$(LIBEXEC_EXTENSIONS)/"
	@echo "installed: $(LIBEXEC_EXTENSIONS) (autoskd discovers feature-dev here automatically)"

## uninstall: remove autosk + autoskd from $$GOBIN (or $$GOPATH/bin)
uninstall:
	@for b in $(BIN_NAME) autoskd; do \
		if [ -f "$(GOBIN_DIR)/$$b" ]; then \
			rm -f "$(GOBIN_DIR)/$$b" && echo "removed: $(GOBIN_DIR)/$$b"; \
		else \
			echo "not installed: $(GOBIN_DIR)/$$b"; \
		fi; \
	done
	@if [ -d "$(LIBEXEC_EXTENSIONS)" ]; then \
		rm -rf "$(LIBEXEC_EXTENSIONS)" && echo "removed: $(LIBEXEC_EXTENSIONS)"; \
	fi

## test: run all tests (compiles autoskd first; the verb tests auto-spawn it)
test: build-autoskd
	AUTOSKD_BIN=$(CURDIR)/$(AUTOSKD_BIN) AUTOSK_BUNDLED_EXTENSIONS=$(BUNDLED_EXTENSIONS) $(GO) test ./...

## test-short: skip long tests
test-short: build-autoskd
	AUTOSKD_BIN=$(CURDIR)/$(AUTOSKD_BIN) AUTOSK_BUNDLED_EXTENSIONS=$(BUNDLED_EXTENSIONS) $(GO) test -short ./...

## lint: run golangci-lint (must be installed)
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed"; exit 1; }
	golangci-lint run ./...

## tidy: go mod tidy
tidy:
	$(GO) mod tidy

## fmt: gofmt
fmt:
	$(GO) fmt ./...

## vet: go vet
vet:
	$(GO) vet ./...

## clean: remove build artifacts
clean:
	rm -rf $(BIN_DIR) dist

## distclean: clean (alias; the Bun daemon keeps no extra build cache here)
distclean: clean

## help: show this help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^##/ {gsub(/^## /, ""); split($$0, a, ":"); printf "  \033[36m%-14s\033[0m %s\n", a[1], a[2]}' $(MAKEFILE_LIST)
