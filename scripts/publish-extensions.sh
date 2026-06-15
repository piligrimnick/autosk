#!/usr/bin/env bash
#
# publish-extensions.sh — publish the @autosk/* extension packages to npm.
#
# autosk ships its reference workflow as ordinary npm packages (there are no
# daemon-bundled extensions): the daemon `npm install`s `@autosk/feature-dev` on
# first run. This script publishes the daemon-chain packages that make that work,
# in dependency order so a consumer's `npm install @autosk/feature-dev` always
# resolves:
#
#   @autosk/sdk  →  @autosk/worktree  →  @autosk/pi-agent  →  @autosk/feature-dev
#
# It also publishes `@autosk/merge-to-current` (a standalone workflow that merges
# a task branch into the project's current branch); it depends only on
# `@autosk/sdk` + `@autosk/pi-agent`, so it publishes after them and is NOT part
# of the first-run bootstrap (install it explicitly with `autosk install`).
#
# It ALSO publishes `@autosk/pi-tools` — the standalone pi extension exposing the
# agent-facing `autosk_task` / `autosk_comment` tools. It is NOT part of the
# daemon install chain (it carries no inter-@autosk deps and is installed into
# pi, not `~/.autosk/packages/`), so its publish order is free; it lives at the
# repo root (`pi-tools/`) with its own deps + checks, outside the daemon Bun
# workspace.
#
# The daemon-chain packages ship RAW TypeScript (the daemon runs on Bun and
# imports .ts natively) plus a root `index.ts` re-export shim — REQUIRED because
# the compiled `autoskd` (`bun build --compile`) resolves an on-disk dependency's
# bare imports only via a root-level `index.ts` (it ignores `exports`/`main`/
# subdir targets). `@autosk/pi-tools` is loaded by pi (not the compiled daemon),
# so it needs no such shim.
#
# Prerequisites:
#   - `npm login` with publish rights to the `@autosk` org (create it on npmjs.com
#     first; the scope must exist).
#   - bump the version of any package you intend to (re)publish; a package whose
#     repo version is already on npm is skipped automatically (inter-package deps
#     use `^x.y.z`, so keep them in sync).
#
# Usage:
#   scripts/publish-extensions.sh            # DRY RUN (npm publish --dry-run)
#   scripts/publish-extensions.sh --publish  # actually publish
#   OTP=123456 scripts/publish-extensions.sh --publish   # with 2FA one-time code
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
daemon="$repo_root/daemon"
npm_bin="${NPM:-npm}"
bun_bin="${BUN:-bun}"

mode="dry"
case "${1:-}" in
  ""|--dry-run) mode="dry" ;;
  --publish)    mode="publish" ;;
  *) echo "usage: publish-extensions.sh [--dry-run|--publish]" >&2; exit 2 ;;
esac

command -v "$npm_bin" >/dev/null 2>&1 || { echo "publish-extensions: npm not found on PATH" >&2; exit 1; }

# registry_has_version <name> <version> — true when that EXACT version is already
# published on npm. Used to skip a package whose repo version is already live
# (otherwise `npm publish` aborts with "cannot publish over the previously
# published versions"). A never-published package / unknown version 404s → empty
# output → treated as not-present, so we proceed. Needs registry access; offline
# it also 404s → proceed (and the publish step then surfaces the real error).
registry_has_version() {
  local name="$1" ver="$2" out
  out="$("$npm_bin" view "$name@$ver" version 2>/dev/null || true)"
  [ "$(printf '%s' "$out" | tr -d '[:space:]')" = "$ver" ]
}

# Publish order = dependency order (deps before dependents). Paths are relative
# to the repo root. `pi-tools` carries no inter-@autosk deps, so its position is
# free; it goes last.
packages=(
  "daemon/sdk"
  "daemon/extensions/worktree"
  "daemon/extensions/pi-agent"
  "daemon/extensions/feature-dev"
  "daemon/extensions/merge-to-current"
  "pi-tools"
)

echo "== sanity: daemon bun install + typecheck + test =="
( cd "$daemon" && "$bun_bin" install --frozen-lockfile >/dev/null 2>&1 || "$bun_bin" install >/dev/null )
( cd "$daemon" && "$bun_bin" run typecheck )
( cd "$daemon" && "$bun_bin" test )

echo "== sanity: pi-tools bun install + typecheck + test =="
( cd "$repo_root/pi-tools" && "$bun_bin" install --frozen-lockfile >/dev/null 2>&1 || "$bun_bin" install >/dev/null )
( cd "$repo_root/pi-tools" && "$bun_bin" run typecheck )
( cd "$repo_root/pi-tools" && "$bun_bin" test )

otp_args=()
[ -n "${OTP:-}" ] && otp_args=(--otp "$OTP")

for rel in "${packages[@]}"; do
  dir="$repo_root/$rel"
  name="$(cd "$dir" && node -p "require('./package.json').name" 2>/dev/null || echo "$rel")"
  ver="$(cd "$dir" && node -p "require('./package.json').version" 2>/dev/null || echo "?")"
  if registry_has_version "$name" "$ver"; then
    echo "== skip: $name@$ver already on npm ($rel) =="
    continue
  fi
  if [ "$mode" = "dry" ]; then
    echo "== DRY RUN: $name@$ver ($rel) =="
    ( cd "$dir" && "$npm_bin" publish --access public --dry-run )
  else
    echo "== publish: $name@$ver ($rel) =="
    ( cd "$dir" && "$npm_bin" publish --access public "${otp_args[@]}" )
  fi
done

if [ "$mode" = "dry" ]; then
  echo
  echo "DRY RUN complete. Re-run with --publish to actually publish (needs \`npm login\`)."
else
  echo
  echo "Published. A fresh machine's daemon will \`npm install @autosk/feature-dev\` on first run;"
  echo "install \`@autosk/pi-tools\` in pi (\`~/.pi/agent/settings.json\`) for the autosk_task / autosk_comment tools."
fi
