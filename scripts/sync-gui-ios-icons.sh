#!/usr/bin/env bash
# sync-gui-ios-icons.sh — copy the committed custom iOS app icons into the
# generated (gitignored) iOS Xcode asset catalog.
#
# WHY
#   `tauri ios init` regenerates gui/src-tauri/gen/apple/ from a template and
#   seeds Assets.xcassets/AppIcon.appiconset/ with the DEFAULT Tauri logo — it
#   does NOT read gui/src-tauri/icons/ios/. So without this step every iOS build
#   (local install-gui-ios.sh AND the CI build-ios job) ships the stock Tauri
#   icon. The custom icon set is generated once by scripts/update-gui-icons.sh
#   and committed under gui/src-tauri/icons/ios/ (Git LFS); this script just
#   consumes those committed PNGs.
#
# WHEN
#   Run AFTER `tauri ios init` and BEFORE `tauri ios build` / opening Xcode.
#   Idempotent. The AppIcon.appiconset PNG filenames match icons/ios/ 1:1, and
#   the template Contents.json is left untouched.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$repo_root/gui/src-tauri/icons/ios"
dst="$repo_root/gui/src-tauri/gen/apple/Assets.xcassets/AppIcon.appiconset"

die() { echo "sync-gui-ios-icons: $*" >&2; exit 1; }

[ -d "$src" ] || die "source icons not found: $src (run scripts/update-gui-icons.sh)"
[ -d "$dst" ] || die "iOS asset catalog not found: $dst (run 'tauri ios init' first)"

count=0
for f in "$src"/*.png; do
  [ -e "$f" ] || continue
  cp -f "$f" "$dst/$(basename "$f")"
  count=$((count + 1))
done

[ "$count" -gt 0 ] || die "no PNG icons found in $src"

echo "sync-gui-ios-icons: synced $count custom app icons -> gen/apple AppIcon.appiconset"
