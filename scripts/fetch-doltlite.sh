#!/usr/bin/env bash
# fetch-doltlite.sh — download a prebuilt libdoltlite + headers from
# upstream GitHub releases and stage them at DEST so CGO can link against
# them. Idempotent: if the destination already has the artifacts, exits 0
# without touching the network.
#
# Usage:
#   scripts/fetch-doltlite.sh <version> <dest-dir>
#
# Environment overrides:
#   DOLTLITE_PLATFORM   force the platform suffix
#                       (osx-arm64 | linux-x64 | linux-arm64 | win-x64)
#   DOLTLITE_URL        explicit zip URL (skips version + platform derivation)
#   DOLTLITE_SHA256     expected sha256 of the downloaded zip; if set,
#                       verification is mandatory and the script fails on
#                       mismatch.
#
# Exit codes:
#   0  destination is ready (was already present, or freshly downloaded)
#   1  any failure (unknown platform, network error, checksum mismatch, ...)

set -euo pipefail

VERSION="${1:-}"
DEST="${2:-}"
if [ -z "$VERSION" ] || [ -z "$DEST" ]; then
    echo "usage: $0 <version> <dest-dir>" >&2
    exit 2
fi

# --- platform detection -----------------------------------------------------

detect_platform() {
    local os arch
    os=$(uname -s)
    arch=$(uname -m)
    case "$os/$arch" in
        Darwin/arm64)        echo "osx-arm64" ;;
        Linux/x86_64)        echo "linux-x64" ;;
        Linux/amd64)         echo "linux-x64" ;;
        Linux/aarch64)       echo "linux-arm64" ;;
        Linux/arm64)         echo "linux-arm64" ;;
        MINGW*/x86_64|MSYS*/x86_64|CYGWIN*/x86_64) echo "win-x64" ;;
        *)
            return 1
            ;;
    esac
}

PLATFORM="${DOLTLITE_PLATFORM:-}"
if [ -z "$PLATFORM" ]; then
    if ! PLATFORM=$(detect_platform); then
        echo "ERROR: cannot auto-detect doltlite platform for $(uname -s)/$(uname -m)." >&2
        echo "       Set DOLTLITE_PLATFORM (osx-arm64|linux-x64|linux-arm64|win-x64)" >&2
        echo "       or point DOLTLITE_DIR at a pre-built doltlite directory." >&2
        exit 1
    fi
fi

# --- idempotency: already staged? ------------------------------------------

if [ -f "$DEST/libdoltlite.a" ] && [ -f "$DEST/sqlite3.h" ]; then
    echo "fetch-doltlite: artifacts already present at $DEST"
    exit 0
fi

# --- download ---------------------------------------------------------------

URL="${DOLTLITE_URL:-https://github.com/dolthub/doltlite/releases/download/v${VERSION}/doltlite-lib-${PLATFORM}-${VERSION}.zip}"

echo "fetch-doltlite: downloading $URL"

TMP=$(mktemp -d 2>/dev/null || mktemp -d -t doltlite)
trap 'rm -rf "$TMP"' EXIT

ZIP="$TMP/doltlite.zip"
if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 --retry-delay 2 -o "$ZIP" "$URL"
elif command -v wget >/dev/null 2>&1; then
    wget --tries=3 --waitretry=2 -q -O "$ZIP" "$URL"
else
    echo "ERROR: neither curl nor wget is available." >&2
    exit 1
fi

# --- optional checksum verification ----------------------------------------

if [ -n "${DOLTLITE_SHA256:-}" ]; then
    if command -v shasum >/dev/null 2>&1; then
        got=$(shasum -a 256 "$ZIP" | awk '{print $1}')
    elif command -v sha256sum >/dev/null 2>&1; then
        got=$(sha256sum "$ZIP" | awk '{print $1}')
    else
        echo "ERROR: DOLTLITE_SHA256 set but no shasum/sha256sum available." >&2
        exit 1
    fi
    if [ "$got" != "$DOLTLITE_SHA256" ]; then
        echo "ERROR: sha256 mismatch for $URL" >&2
        echo "  expected: $DOLTLITE_SHA256" >&2
        echo "  actual:   $got" >&2
        exit 1
    fi
    echo "fetch-doltlite: sha256 OK ($got)"
fi

# --- unpack -----------------------------------------------------------------

if ! command -v unzip >/dev/null 2>&1; then
    echo "ERROR: 'unzip' is required but not installed." >&2
    exit 1
fi

unzip -q "$ZIP" -d "$TMP/unpack"

# The release zips contain a single top-level directory like
#   doltlite-lib-osx-arm64-0.10.11/
SRC_DIR=$(find "$TMP/unpack" -mindepth 1 -maxdepth 1 -type d | head -n 1)
if [ -z "$SRC_DIR" ] || [ ! -d "$SRC_DIR" ]; then
    echo "ERROR: unexpected zip layout (no top-level directory)." >&2
    exit 1
fi

# Stage into a sibling directory and rename — this avoids leaving a half-
# populated DEST if anything fails mid-copy.
STAGE="${DEST}.staging.$$"
rm -rf "$STAGE"
mkdir -p "$STAGE"

cp "$SRC_DIR/libdoltlite.a" "$STAGE/"
cp "$SRC_DIR/doltlite.h"    "$STAGE/"

# mattn/go-sqlite3 (used here via -tags libsqlite3) includes <sqlite3.h>.
# Upstream installs ship both names with identical content; the release zip
# only ships doltlite.h, so we duplicate it so -I$DEST works either way.
cp "$STAGE/doltlite.h" "$STAGE/sqlite3.h"

# Optional extras — copy if present, ignore if not.
for f in doltlite_remotesrv.h libdoltlite.dylib libdoltlite.so libdoltlite.dll; do
    if [ -f "$SRC_DIR/$f" ]; then
        cp "$SRC_DIR/$f" "$STAGE/"
    fi
done

# Marker so other tools / future doctor runs can introspect the cache.
printf 'version=%s\nplatform=%s\nurl=%s\n' "$VERSION" "$PLATFORM" "$URL" \
    > "$STAGE/.doltlite-fetch.txt"

# --- atomic swap ------------------------------------------------------------

mkdir -p "$(dirname "$DEST")"
rm -rf "$DEST"
mv "$STAGE" "$DEST"

echo "fetch-doltlite: doltlite $VERSION ($PLATFORM) installed at $DEST"
