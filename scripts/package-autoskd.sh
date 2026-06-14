#!/usr/bin/env bash
#
# package-autoskd.sh — build the distributable autoskd layout for the current
# host platform.
#
# The Bun daemon is compiled to a standalone binary with `bun build --compile`
# (it embeds the Bun runtime, so there is NO global bun at runtime). There are no
# daemon-bundled extensions: the reference `feature-dev` workflow ships as an
# npm package (`@autosk/feature-dev`) that the daemon installs into
# ~/.autosk/packages/ on first run (ensureGlobalBootstrap), so nothing but the
# binary needs to be packaged.
#
# Usage:
#   scripts/package-autoskd.sh <out-dir>
#
# Produces:
#
#   <out-dir>/
#     bin/autoskd   the compiled daemon
#
# Requires `bun` on PATH (build time only).
set -euo pipefail

out="${1:?usage: package-autoskd.sh <out-dir>}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
daemon="$repo_root/daemon"
bun="${BUN:-bun}"
command -v "$bun" >/dev/null 2>&1 || { echo "package-autoskd: bun not found on PATH" >&2; exit 1; }

mkdir -p "$out/bin"

echo "package-autoskd: bun install (workspace)"
(cd "$daemon" && "$bun" install --frozen-lockfile >/dev/null 2>&1 || "$bun" install)

# Bake the release version/commit into the binary when provided (release.yml
# exports them from the tag); otherwise the daemon reports 0.0.0-dev.
defines=()
[ -n "${AUTOSK_VERSION:-}" ] && defines+=(--define "process.env.AUTOSK_VERSION=\"${AUTOSK_VERSION}\"")
[ -n "${AUTOSK_COMMIT:-}" ]  && defines+=(--define "process.env.AUTOSK_COMMIT=\"${AUTOSK_COMMIT}\"")

echo "package-autoskd: bun build --compile -> $out/bin/autoskd"
(cd "$daemon" && "$bun" build --compile core/src/index.ts ${defines[@]+"${defines[@]}"} --outfile "$out/bin/autoskd")

echo "package-autoskd: done -> $out"
