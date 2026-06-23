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
#   @autosk/sdk  →  @autosk/sandbox  →  @autosk/pi-agent  →  @autosk/feature-dev
#
# It also publishes the other shipped @autosk extensions, each after its deps:
#   - `@autosk/claude-agent` (agent: drives Claude Code; deps @autosk/sdk).
#   - `@autosk/sandbox` (userspace sandbox library: worktreeSandbox() /
#     dockerSandbox() / sandboxCleanupStep(); deps @autosk/sdk).
#   - `@autosk/feature-dev-cc` (the feature-dev workflow wired to the Claude Code
#     agent; deps @autosk/sdk + @autosk/sandbox + @autosk/claude-agent).
#   - `@autosk/feature-dev-docker` (the feature-dev pi workflow with every agent
#     step in a per-task dockerSandbox; deps @autosk/sdk + @autosk/sandbox +
#     @autosk/feature-dev).
#   - `@autosk/merge-to-current` (a standalone workflow that merges a task branch
#     into the project's current branch; deps @autosk/sdk + @autosk/pi-agent).
#
# It ALSO builds + pushes the thin operator DOCKER IMAGES to GHCR (multi-arch via
# buildx): `ghcr.io/wierdbytes/pi-runtime` (from daemon/extensions/pi-agent/docker)
# and `ghcr.io/wierdbytes/claude-runtime` (from daemon/extensions/claude-agent/docker)
# — the images `dockerSandbox({ image })` runs. Needs `docker login ghcr.io` (a
# token with write:packages); skip with `--no-images`.
# None of these are part of the first-run bootstrap (only @autosk/feature-dev is);
# install them explicitly with `autosk ext add`.
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
#   scripts/publish-extensions.sh            # DRY RUN (npm publish --dry-run + image plan)
#   scripts/publish-extensions.sh --publish  # publish npm packages + push GHCR images
#   scripts/publish-extensions.sh --publish --no-images  # npm only, skip the images
#   OTP=123456 scripts/publish-extensions.sh --publish   # with 2FA one-time code
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
daemon="$repo_root/daemon"
npm_bin="${NPM:-npm}"
bun_bin="${BUN:-bun}"

mode="dry"
do_images=1
for arg in "$@"; do
  case "$arg" in
    --dry-run) mode="dry" ;;
    --publish) mode="publish" ;;
    --no-images) do_images=0 ;;
    *) echo "usage: publish-extensions.sh [--dry-run|--publish] [--no-images]" >&2; exit 2 ;;
  esac
done

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
  "daemon/extensions/sandbox"
  "daemon/extensions/pi-agent"
  "daemon/extensions/claude-agent"
  "daemon/extensions/feature-dev"
  "daemon/extensions/feature-dev-docker"
  "daemon/extensions/feature-dev-cc"
  "daemon/extensions/merge-to-current"
  "pi-tools"
)

# Operator docker images: "<ghcr image>=<build context dir>" (built multi-arch +
# pushed by publish_images). The build context is the extension's `docker/` dir.
images=(
  "ghcr.io/wierdbytes/pi-runtime=daemon/extensions/pi-agent/docker"
  "ghcr.io/wierdbytes/claude-runtime=daemon/extensions/claude-agent/docker"
)

# publish_images <dry|publish> — build + push each image to GHCR (multi-arch via
# buildx). Needs `docker login ghcr.io` and a docker with buildx; a missing docker
# is a non-fatal skip. Tags come from $IMAGE_TAGS (space-separated; e.g.
# "latest sha-abc123" for rollback pins) and default to $IMAGE_TAG (default
# latest); platforms via $IMAGE_PLATFORMS (default linux/amd64,linux/arm64). All
# tags are pushed from a SINGLE buildx build (multiple `-t` flags).
publish_images() {
  local m="$1" platforms="${IMAGE_PLATFORMS:-linux/amd64,linux/arm64}"
  local tags="${IMAGE_TAGS:-${IMAGE_TAG:-latest}}"
  local docker_bin="${DOCKER:-docker}"
  if ! command -v "$docker_bin" >/dev/null 2>&1; then
    echo "== skip images: $docker_bin not on PATH =="; return 0
  fi
  local builder="autosk-images"
  for entry in "${images[@]}"; do
    local image="${entry%%=*}" ctx="$repo_root/${entry#*=}"
    # One image, many tags → one build with repeated `-t image:tag`.
    local tag_args=() t
    for t in $tags; do tag_args+=(-t "$image:$t"); done
    if [ "$m" = "dry" ]; then
      echo "== DRY RUN image: $image [$tags] ($ctx) for $platforms =="
      continue
    fi
    echo "== publish image: $image [$tags] ($ctx) for $platforms =="
    if ! "$docker_bin" buildx inspect "$builder" >/dev/null 2>&1; then
      "$docker_bin" buildx create --name "$builder" --use >/dev/null
    else
      "$docker_bin" buildx use "$builder"
    fi
    "$docker_bin" buildx build --platform "$platforms" "${tag_args[@]}" --push "$ctx"
  done
}

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

if [ "$do_images" = 1 ]; then
  echo
  echo "== docker images (GHCR) =="
  publish_images "$mode"
else
  echo "== skip images (--no-images) =="
fi

if [ "$mode" = "dry" ]; then
  echo
  echo "DRY RUN complete. Re-run with --publish to actually publish (needs \`npm login\` +"
  echo "\`docker login ghcr.io\` for the images; add --no-images to skip them)."
else
  echo
  echo "Published. A fresh machine's daemon will \`npm install @autosk/feature-dev\` on first run;"
  echo "install \`@autosk/pi-tools\` in pi (\`~/.pi/agent/settings.json\`) for the autosk_task / autosk_comment tools."
fi
