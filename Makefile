# autosk — Makefile
#
# Builds against doltlite via CGO. Override DOLTLITE_DIR to point at a
# different doltlite build directory.

DOLTLITE_DIR ?= $(HOME)/me/dev/doltlite/build

# Build tag selects the libsqlite3-link path inside mattn/go-sqlite3 so it
# uses the C library we point CGO at, instead of the embedded amalgamation.
GO_TAGS := libsqlite3

# CGO flags wire mattn/go-sqlite3 to libdoltlite.
export CGO_CFLAGS  := -I$(DOLTLITE_DIR)
export CGO_LDFLAGS := $(DOLTLITE_DIR)/libdoltlite.a -lz -lpthread

GO       ?= go
BIN_DIR  := bin
BIN      := $(BIN_DIR)/autosk

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS  := -X 'autosk/internal/buildinfo.Version=$(VERSION)' \
            -X 'autosk/internal/buildinfo.Commit=$(COMMIT)'

.PHONY: all build test test-short lint doctor clean tidy fmt vet help

all: build

## build: compile bin/autosk
build: doctor
	@mkdir -p $(BIN_DIR)
	$(GO) build -tags $(GO_TAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/autosk

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

## doctor: verify doltlite library is available
doctor:
	@if [ ! -f "$(DOLTLITE_DIR)/sqlite3.h" ]; then \
		echo "ERROR: $(DOLTLITE_DIR)/sqlite3.h not found."; \
		echo "Build doltlite first:"; \
		echo "  cd \$$HOME/me/dev/doltlite && mkdir -p build && cd build && ../configure && make doltlite-lib"; \
		echo "Or set DOLTLITE_DIR to a different location."; \
		exit 1; \
	fi
	@if [ ! -f "$(DOLTLITE_DIR)/libdoltlite.a" ]; then \
		echo "ERROR: $(DOLTLITE_DIR)/libdoltlite.a not found."; \
		echo "Build doltlite first (see above)."; \
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

## clean: remove build artifacts
clean:
	rm -rf $(BIN_DIR) dist

## help: show this help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^##/ {gsub(/^## /, ""); split($$0, a, ":"); printf "  \033[36m%-12s\033[0m %s\n", a[1], a[2]}' $(MAKEFILE_LIST)
