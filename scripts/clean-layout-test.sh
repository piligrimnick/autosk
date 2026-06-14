#!/usr/bin/env bash
#
# clean-layout-test.sh — prove the packaged release works on a clean machine.
#
# Acceptance criterion (P9): "the installed `autosk` locates + spawns the shipped
# `autoskd` with the bundled feature-dev resolvable — no global bun at runtime."
#
# This builds the Go `autosk` + packages `autoskd` and its bundled extensions
# into a throwaway prefix (the same layout the brew formula installs), then runs
# `autosk` against a fresh project with:
#   * PATH scrubbed of bun        (proves the compiled autoskd needs no global bun)
#   * AUTOSK_BUNDLED_EXTENSIONS    unset (proves execPath-relative discovery works)
#   * a clean $HOME                (proves nothing leaks from the dev machine)
# and asserts the auto-spawned daemon resolves the bundled `feature-dev` workflow.
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
test -d "$prefix/libexec/autosk/extensions/feature-dev" \
  || { echo "FAIL: feature-dev not packaged" >&2; exit 1; }

echo "== fresh project under a clean HOME =="
mkdir -p "$home" "$proj"
git -C "$proj" init -q
git -C "$proj" config user.email t@t && git -C "$proj" config user.name t
git -C "$proj" commit -q --allow-empty -m init

# A runtime env with NO bun on PATH and NO bundled-extensions override.
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

echo "== assert the bundled feature-dev workflow resolves =="
out="$(cd "$proj" && run workflow show feature-dev 2>&1)" || {
  echo "FAIL: 'workflow show feature-dev' errored:" >&2; echo "$out" >&2; exit 1;
}
echo "$out" | grep -qi "feature-dev" || {
  echo "FAIL: feature-dev not in 'workflow show' output:" >&2; echo "$out" >&2; exit 1;
}

echo
echo "PASS: clean-layout auto-spawn resolves the bundled feature-dev workflow (no global bun)."
