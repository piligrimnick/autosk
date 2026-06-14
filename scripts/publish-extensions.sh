#!/usr/bin/env bash
#
# publish-extensions.sh — publish the @autosk/* extension packages to npm.
#
# autosk ships its reference workflow as ordinary npm packages (there are no
# daemon-bundled extensions): the daemon `npm install`s `@autosk/feature-dev` on
# first run. This script publishes the four packages that make that work, in
# dependency order so a consumer's `npm install @autosk/feature-dev` always
# resolves:
#
#   @autosk/sdk  →  @autosk/worktree  →  @autosk/pi-agent  →  @autosk/feature-dev
#
# Each package ships RAW TypeScript (the daemon runs on Bun and imports .ts
# natively) plus a root `index.ts` re-export shim — REQUIRED because the compiled
# `autoskd` (`bun build --compile`) resolves an on-disk dependency's bare imports
# only via a root-level `index.ts` (it ignores `exports`/`main`/subdir targets).
#
# Prerequisites:
#   - `npm login` with publish rights to the `@autosk` org (create it on npmjs.com
#     first; the scope must exist).
#   - all four package versions bumped + in sync (inter-package deps use `^x.y.z`).
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

# Publish order = dependency order (deps before dependents).
packages=(
  "sdk"
  "extensions/worktree"
  "extensions/pi-agent"
  "extensions/feature-dev"
)

echo "== sanity: bun install + typecheck + test =="
( cd "$daemon" && "$bun_bin" install --frozen-lockfile >/dev/null 2>&1 || "$bun_bin" install >/dev/null )
( cd "$daemon" && "$bun_bin" run typecheck )
( cd "$daemon" && "$bun_bin" test )

otp_args=()
[ -n "${OTP:-}" ] && otp_args=(--otp "$OTP")

for rel in "${packages[@]}"; do
  dir="$daemon/$rel"
  name="$(cd "$dir" && node -p "require('./package.json').name" 2>/dev/null || echo "$rel")"
  ver="$(cd "$dir" && node -p "require('./package.json').version" 2>/dev/null || echo "?")"
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
  echo "Published. A fresh machine's daemon will now \`npm install @autosk/feature-dev\` on first run."
fi
