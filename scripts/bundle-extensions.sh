#!/usr/bin/env bash
#
# bundle-extensions.sh — build the self-contained daemon-bundled extensions tree.
#
# The compiled autoskd (`bun build --compile`) cannot resolve an on-disk
# extension's bare `@autosk/*` imports: a standalone binary freezes module
# resolution to its embedded graph and does NOT walk on-disk node_modules for a
# dynamically-imported file. So each bundled extension is compiled into ONE
# self-contained file with `bun build` (its `@autosk/*` deps inlined) and shipped
# beside the daemon; the extension loader discovers it via the bundled dir.
#
# Usage:
#   scripts/bundle-extensions.sh <out-ext-dir>
#
# Produces <out-ext-dir>/feature-dev/ :
#   src/index.js                 bundled (deps inlined) — the discoverable entry
#   src/pi-transit-extension.ts  copied verbatim (loaded by pi, NOT the daemon)
#   prompts/*.md                 role prompts (read at load relative to src/index.js)
#   package.json                 { autosk.extensions: ["./src/index.js"] }
#
# Requires `bun` on PATH (build time only).
set -euo pipefail

out="${1:?usage: bundle-extensions.sh <out-ext-dir>}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
daemon="$repo_root/daemon"
bun="${BUN:-bun}"
command -v "$bun" >/dev/null 2>&1 || { echo "bundle-extensions: bun not found on PATH" >&2; exit 1; }

(cd "$daemon" && "$bun" install --frozen-lockfile >/dev/null 2>&1 || "$bun" install)

fd="$out/feature-dev"
mkdir -p "$fd/src"

echo "bundle-extensions: bun build feature-dev -> $fd/src/index.js"
# `--target bun` keeps `node:*` imports external (present in the daemon's
# embedded runtime); the `@autosk/*` deps are inlined into the single file. The
# entry stays at `src/index.js` so feature-dev's `new URL("../prompts/…",
# import.meta.url)` and pi-agent's `new URL("./pi-transit-extension.ts", …)`
# resolve to the shipped siblings below.
(cd "$daemon" && "$bun" build extensions/feature-dev/src/index.ts --target bun --outfile "$fd/src/index.js")

# pi loads this file (not the daemon) via `pi -e`, so it stays a standalone `.ts`
# beside the bundled entry; pi resolves its `@earendil-works/*` / `typebox`
# imports from pi's own environment.
cp "$daemon/extensions/pi-agent/src/pi-transit-extension.ts" "$fd/src/pi-transit-extension.ts"
cp -R "$daemon/extensions/feature-dev/prompts" "$fd/prompts"

cat > "$fd/package.json" <<'JSON'
{
  "name": "@autosk/feature-dev",
  "version": "0.0.0",
  "type": "module",
  "description": "Bundled autoskd reference workflow (feature-dev). Built by scripts/bundle-extensions.sh; @autosk/* deps inlined so the compiled daemon can load it.",
  "autosk": { "extensions": ["./src/index.js"] }
}
JSON

echo "bundle-extensions: done -> $out"
