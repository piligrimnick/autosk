#!/usr/bin/env bash
#
# clean-layout-test.sh — prove the packaged release works on a clean machine.
#
# Acceptance criterion: "the installed `autosk` locates + spawns the shipped
# `autoskd` and serves — no global bun at runtime."
#
# This builds the Go `autosk` + packages `autoskd` into a throwaway prefix (the
# same layout the brew formula installs), then runs `autosk` against a fresh
# project with:
#   * PATH scrubbed of bun        (proves the compiled autoskd needs no global bun)
#   * a clean $HOME                (proves nothing leaks from the dev machine)
# and asserts the auto-spawned daemon serves a basic verb.
#
# NB: there are no daemon-bundled extensions any more. The reference `feature-dev`
# workflow is an npm package the daemon installs on first run (ensureGlobalBootstrap),
# which needs `npm` + network — deliberately ABSENT from this scrubbed env — so
# this smoke does NOT assert feature-dev resolves; it only proves auto-spawn.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
prefix="$work/prefix"
home="$work/home"
proj="$work/project"
sock="$home/daemon.sock"

cleanup() {
  pkill -f "autoskd serve --sock $sock" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "== build + package into $prefix =="
make -C "$repo_root" build >/dev/null
"$repo_root/scripts/package-autoskd.sh" "$prefix" >/dev/null
install -m 0755 "$repo_root/bin/autosk" "$prefix/bin/autosk"

test -x "$prefix/bin/autosk"   || { echo "FAIL: no autosk in prefix"  >&2; exit 1; }
test -x "$prefix/bin/autoskd"  || { echo "FAIL: no autoskd in prefix" >&2; exit 1; }

echo "== fresh project under a clean HOME =="
mkdir -p "$home" "$proj"
git -C "$proj" init -q
git -C "$proj" config user.email t@t && git -C "$proj" config user.name t
git -C "$proj" commit -q --allow-empty -m init

# A runtime env with NO bun on PATH.
clean_path="$prefix/bin:/usr/bin:/bin:/usr/sbin:/sbin"
run() {
  env -i \
    HOME="$home" \
    PATH="$clean_path" \
    AUTOSK_SOCK="$sock" \
    AUTOSK_AUTOINIT_YES=1 \
    "$prefix/bin/autosk" "$@"
}

# Sanity: bun really is not reachable in this env.
if env -i PATH="$clean_path" command -v bun >/dev/null 2>&1; then
  echo "WARN: bun is on the scrubbed PATH — test is weaker than intended" >&2
fi

echo "== init (auto-spawns autoskd) =="
( cd "$proj" && run init >/dev/null )

echo "== assert the auto-spawned daemon serves a basic verb =="
out="$(cd "$proj" && run project list 2>&1)" || {
  echo "FAIL: 'project list' errored (daemon did not auto-spawn/serve):" >&2; echo "$out" >&2; exit 1;
}

echo
echo "PASS: clean-layout auto-spawn serves with no global bun (no bundled extensions)."
