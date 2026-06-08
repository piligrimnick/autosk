# autosk — Makefile
#
# The Go binary (CLI + lazy TUI) is a pure JSON-RPC client of autoskd and no
# longer links doltlite: it builds with plain `go build`, CGO-free, with no
# `-tags libsqlite3` and no libdoltlite.a. autoskd (Rust) is the sole doltlite
# consumer; it fetches its own pinned doltlite via crates/autosk-core/build.rs
# (`scripts/fetch-doltlite.sh 0.11.8`), independently of this Makefile.

GO       ?= go
BIN_DIR  := bin
BIN_NAME := autosk
BIN      := $(BIN_DIR)/$(BIN_NAME)
PKG      := ./cmd/autosk

# autoskd — the Rust daemon that owns .autosk/db. The CLI/lazy front ends are
# pure RPC clients, so the cmd/autosk verb tests need a live daemon; they
# locate it via $AUTOSKD_BIN (the connector's first lookup). Built with cargo.
CARGO       ?= cargo
AUTOSKD_BIN         := $(CURDIR)/target/debug/autoskd
AUTOSKD_RELEASE_BIN := $(CURDIR)/target/release/autoskd

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

.PHONY: all build build-autoskd install install-autoskd uninstall test test-short lint \
        clean distclean tidy fmt vet help

all: build

## build: compile bin/autosk (CGO-free; no doltlite)
build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## build-autoskd: compile the Rust daemon (needed by the cmd/autosk verb tests)
build-autoskd:
	$(CARGO) build -p autoskd

## install: install autosk + autoskd into $$GOBIN (or $$GOPATH/bin)
install: install-autoskd
	$(GO) install -ldflags "$(LDFLAGS)" $(PKG)
	@echo "installed: $(GOBIN_DIR)/$(BIN_NAME)"

## install-autoskd: build autoskd (release) and install it alongside autosk
install-autoskd:
	$(CARGO) build -p autoskd --release
	@mkdir -p "$(GOBIN_DIR)"
	@install -m 0755 "$(AUTOSKD_RELEASE_BIN)" "$(GOBIN_DIR)/autoskd"
	@echo "installed: $(GOBIN_DIR)/autoskd"

## uninstall: remove autosk + autoskd from $$GOBIN (or $$GOPATH/bin)
uninstall:
	@for b in $(BIN_NAME) autoskd; do \
		if [ -f "$(GOBIN_DIR)/$$b" ]; then \
			rm -f "$(GOBIN_DIR)/$$b" && echo "removed: $(GOBIN_DIR)/$$b"; \
		else \
			echo "not installed: $(GOBIN_DIR)/$$b"; \
		fi; \
	done

## test: run all tests (builds autoskd first; the verb tests auto-spawn it)
test: build-autoskd
	AUTOSKD_BIN=$(AUTOSKD_BIN) $(GO) test ./...

## test-short: skip long tests
test-short: build-autoskd
	AUTOSKD_BIN=$(AUTOSKD_BIN) $(GO) test -short ./...

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

## distclean: clean + drop the downloaded doltlite cache (Rust autoskd refetches)
distclean: clean
	rm -rf $(CURDIR)/.doltlite

## help: show this help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^##/ {gsub(/^## /, ""); split($$0, a, ":"); printf "  \033[36m%-14s\033[0m %s\n", a[1], a[2]}' $(MAKEFILE_LIST)
