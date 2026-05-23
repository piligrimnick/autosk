# autosk — Makefile
#
# Builds against doltlite via CGO. By default the required libdoltlite.a +
# headers are downloaded from upstream GitHub releases into ./.doltlite the
# first time you build, with no dependency on a local doltlite checkout.
#
# To use a locally-built doltlite (e.g. when working on unreleased changes),
# point DOLTLITE_DIR at its build directory before invoking make:
#
#     make build DOLTLITE_DIR=$HOME/me/dev/doltlite/build
#
# When DOLTLITE_DIR is supplied externally, the auto-fetch is skipped and
# `doctor` only verifies that the expected files exist there.

# -----------------------------------------------------------------------------
# Doltlite acquisition
# -----------------------------------------------------------------------------

# Pinned upstream release. Bump this when the doltlite API changes.
#
# Why 0.10.8 and not the newest:
#   - v0.10.9 introduced a per-ChunkStore pthread_mutex (PR #958) that
#     deadlocks against Go's goroutine/OS-thread migration: a goroutine
#     that acquires the mutex on thread T1 and re-enters via CGo on
#     thread T2 sees SQLITE_BUSY forever and trips our 30s busy_timeout
#     with "database is locked".
#   - v0.10.11 additionally corrupts the schema-cookie reload after
#     dolt_gc()'s atomic rename (PR #1005 + 0d589b5195), leaving the
#     on-disk file unreadable to a fresh sqlite3_open ("malformed
#     database schema (idx_runs_status) - invalid rootpage"). The
#     daemon's periodic Compact() would brick the DB.
#
# v0.10.8 is the last release before either regression. Revisit once
# upstream ships a fix; track this in docs/plans (TBD).
DOLTLITE_VERSION ?= 0.10.8

# Platform suffix used in release asset names. Auto-detected from uname; can
# be overridden, e.g. for cross-fetching on a foreign host.
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_S),Darwin)
  ifeq ($(UNAME_M),arm64)
    DOLTLITE_PLATFORM ?= osx-arm64
  endif
endif
ifeq ($(UNAME_S),Linux)
  ifeq ($(UNAME_M),x86_64)
    DOLTLITE_PLATFORM ?= linux-x64
  endif
  ifeq ($(UNAME_M),aarch64)
    DOLTLITE_PLATFORM ?= linux-arm64
  endif
  ifeq ($(UNAME_M),arm64)
    DOLTLITE_PLATFORM ?= linux-arm64
  endif
endif
DOLTLITE_PLATFORM ?= unknown

# Capture how DOLTLITE_DIR was supplied BEFORE we assign a default, so we can
# tell whether the user explicitly picked the install location (env / CLI) or
# is happy with our managed cache.
DOLTLITE_DIR_ORIGIN := $(origin DOLTLITE_DIR)

# In-tree cache for the fetched library, namespaced by version + platform so
# multiple checkouts or platform switches don't fight over the same files.
DOLTLITE_DIR ?= $(CURDIR)/.doltlite/$(DOLTLITE_VERSION)-$(DOLTLITE_PLATFORM)

FETCH_SCRIPT := $(CURDIR)/scripts/fetch-doltlite.sh

# -----------------------------------------------------------------------------
# Go / CGO wiring
# -----------------------------------------------------------------------------

# Build tag selects the libsqlite3-link path inside mattn/go-sqlite3 so it
# uses the C library we point CGO at, instead of the embedded amalgamation.
GO_TAGS := libsqlite3

# CGO flags wire mattn/go-sqlite3 to libdoltlite.
#
# -lm is required on Linux: libdoltlite.a pulls in sqlite3 math functions
# (log/log2/pow/sin/cos/exp/sqrt/expm1/...) and prolly_hash's Weibull check,
# which on glibc live in libm. macOS folds libm into libSystem, so -lm is a
# harmless no-op there.
export CGO_CFLAGS  := -I$(DOLTLITE_DIR)
export CGO_LDFLAGS := $(DOLTLITE_DIR)/libdoltlite.a -lz -lpthread -lm

GO       ?= go
BIN_DIR  := bin
BIN_NAME := autosk
BIN      := $(BIN_DIR)/$(BIN_NAME)
PKG      := ./cmd/autosk

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

.PHONY: all build install uninstall test test-short lint doctor fetch-doltlite \
        clean distclean tidy fmt vet help

all: build

## build: compile bin/autosk
build: doctor
	@mkdir -p $(BIN_DIR)
	$(GO) build -tags $(GO_TAGS) -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## install: install autosk into $$GOBIN (or $$GOPATH/bin)
install: doctor
	$(GO) install -tags $(GO_TAGS) -ldflags "$(LDFLAGS)" $(PKG)
	@echo "installed: $(GOBIN_DIR)/$(BIN_NAME)"

## uninstall: remove autosk from $$GOBIN (or $$GOPATH/bin)
uninstall:
	@if [ -f "$(GOBIN_DIR)/$(BIN_NAME)" ]; then \
		rm -f "$(GOBIN_DIR)/$(BIN_NAME)" && \
		echo "removed: $(GOBIN_DIR)/$(BIN_NAME)"; \
	else \
		echo "not installed: $(GOBIN_DIR)/$(BIN_NAME)"; \
	fi

## test: run all tests
test: doctor
	$(GO) test -tags $(GO_TAGS) ./...

## test-short: skip long tests
test-short: doctor
	$(GO) test -tags $(GO_TAGS) -short ./...

## lint: run golangci-lint (must be installed)
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed"; exit 1; }
	golangci-lint run --build-tags $(GO_TAGS) ./...

## fetch-doltlite: download libdoltlite + headers into $(DOLTLITE_DIR)
fetch-doltlite:
	@DOLTLITE_PLATFORM='$(DOLTLITE_PLATFORM)' bash '$(FETCH_SCRIPT)' '$(DOLTLITE_VERSION)' '$(DOLTLITE_DIR)'

# Only auto-fetch when the user did NOT supply DOLTLITE_DIR. With a user
# override we assume the directory is managed externally (e.g. a local
# doltlite build) and limit ourselves to verification in `doctor`.
ifeq ($(DOLTLITE_DIR_ORIGIN),undefined)
doctor: fetch-doltlite
endif

## doctor: verify doltlite library is available at $(DOLTLITE_DIR)
doctor:
	@if [ ! -f "$(DOLTLITE_DIR)/sqlite3.h" ]; then \
		echo "ERROR: $(DOLTLITE_DIR)/sqlite3.h not found."; \
		echo "  - Default flow: run 'make fetch-doltlite' to download doltlite $(DOLTLITE_VERSION)."; \
		echo "  - Custom flow:  set DOLTLITE_DIR to a directory containing a locally-built doltlite."; \
		exit 1; \
	fi
	@if [ ! -f "$(DOLTLITE_DIR)/libdoltlite.a" ]; then \
		echo "ERROR: $(DOLTLITE_DIR)/libdoltlite.a not found."; \
		echo "  - Default flow: run 'make fetch-doltlite'."; \
		echo "  - Custom flow:  set DOLTLITE_DIR to your local doltlite build."; \
		exit 1; \
	fi
	@echo "doctor: doltlite OK at $(DOLTLITE_DIR)"

## tidy: go mod tidy
tidy:
	$(GO) mod tidy

## fmt: gofmt
fmt:
	$(GO) fmt ./...

## vet: go vet
vet: doctor
	$(GO) vet -tags $(GO_TAGS) ./...

## clean: remove build artifacts (keeps the doltlite cache)
clean:
	rm -rf $(BIN_DIR) dist

## distclean: clean + drop the downloaded doltlite cache
distclean: clean
	rm -rf $(CURDIR)/.doltlite

## help: show this help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^##/ {gsub(/^## /, ""); split($$0, a, ":"); printf "  \033[36m%-14s\033[0m %s\n", a[1], a[2]}' $(MAKEFILE_LIST)
