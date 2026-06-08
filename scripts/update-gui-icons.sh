#!/usr/bin/env bash
# update-gui-icons.sh — regenerate the autosk GUI (Tauri) app icons from an
# Apple Icon Composer ".icon" file plus a 1024x1024 PNG export.
#
# ---------------------------------------------------------------------------
# USAGE
#   # regenerate from the sources committed in the repo (the common case):
#   scripts/update-gui-icons.sh
#
#   # adopt a NEW icon from external files (also refreshes the stored sources
#   # under gui/src-tauri/icons/src/ so the repo copies stay authoritative):
#   scripts/update-gui-icons.sh <path/to/App.icon> <path/to/icon-1024.png>
#
# EXAMPLE
#   scripts/update-gui-icons.sh ~/Downloads/autosk.icon ~/Downloads/autosk.png
#
# Run from anywhere; paths may be relative or absolute. The script writes into
# gui/src-tauri/icons/ inside this repo.
#
# SOURCES OF TRUTH (committed; stored in Git LFS)
#   gui/src-tauri/icons/src/autosk.icon  — Icon Composer bundle (Liquid Glass)
#   gui/src-tauri/icons/src/autosk.png   — 1024x1024 flat export
#   With no args the script regenerates from these. With two args it copies the
#   given files over them first. (Binary icon artifacts under icons/ are tracked
#   via gui/src-tauri/icons/.gitattributes — contributors need `git lfs install`.)
# ---------------------------------------------------------------------------
# WHY TWO INPUTS?
#   A ".icon" is an Icon Composer *bundle* (a folder of vector layers + a
#   icon.json describing the Liquid Glass treatment) — it is NOT a ready image.
#   We need it in two different shapes:
#
#     1. Assets.car  — the compiled Liquid Glass icon for macOS 26+. Produced
#        from the .icon by `actool`.
#     2. A flat .icns/.ico/PNG set — the cross-platform fallback (macOS < 26,
#        Windows, Linux). Produced by `tauri icon` from a 1024x1024 PNG.
#
#   The flat set needs a crisp, properly-masked 1024 image. The standalone
#   .icns that actool emits caps at 256px, and the raw layer art inside the
#   .icon has opaque (white) corners — neither is usable directly. So you must
#   also export a 1024x1024 PNG from Icon Composer (it applies the squircle
#   mask, transparency and margins for you).
#
# HOW TO PRODUCE THE INPUTS (Apple Icon Composer)
#   1. Design + save the icon  -> gives you "App.icon".
#   2. File -> Export -> PNG (1024x1024) -> gives you the flat fallback PNG.
# ---------------------------------------------------------------------------
# WHAT THIS SCRIPT DOES
#   1. Copies the input .icon to a temp "autosk.icon" so the compiled asset is
#      ALWAYS named "autosk" (regardless of the input filename), matching
#      CFBundleIconName in gui/src-tauri/Info.plist.
#   2. actool compiles it -> gui/src-tauri/icons/Assets.car (Liquid Glass).
#   3. `tauri icon <png>` regenerates the flat .icns/.ico/PNG set in
#      gui/src-tauri/icons/.
#   4. Verifies the (already committed) wiring is still in place and warns if
#      not — it does NOT edit tauri.conf.json / Info.plist itself.
#
# WIRING (committed once; this script only refreshes the assets)
#   - gui/src-tauri/Info.plist       -> <key>CFBundleIconName</key> <string>autosk</string>
#                                       (macOS 26+ picks Assets.car via this key;
#                                        macOS < 26 ignores it and uses icon.icns)
#   - gui/src-tauri/tauri.conf.json  -> bundle.macOS.files maps
#                                       "Resources/Assets.car": "icons/Assets.car"
#                                       (copies Assets.car into the .app bundle)
#
# REQUIREMENTS
#   - macOS with Xcode installed: `actool` ships with Xcode and is required to
#     compile the .icon. Once Assets.car is committed, building the app on a
#     machine WITHOUT Xcode (e.g. CI) works fine.
#   - GUI deps installed (gui/node_modules) so `npx tauri icon` resolves.
#
# VERIFY THE RESULT
#   cd gui && npm run tauri build -- --debug --bundles app
#   then confirm:
#     - <app>/Contents/Resources/Assets.car exists
#     - /usr/libexec/PlistBuddy -c 'Print :CFBundleIconName' <app>/Contents/Info.plist  => autosk
# ---------------------------------------------------------------------------
set -euo pipefail

ASSET_NAME="autosk"   # MUST match CFBundleIconName in gui/src-tauri/Info.plist

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUI="$ROOT/gui"
ICONS_DIR="$GUI/src-tauri/icons"
PLIST="$GUI/src-tauri/Info.plist"
CONF="$GUI/src-tauri/tauri.conf.json"

die() { echo "ERROR: $*" >&2; exit 1; }

# Resolve a path to absolute (no symlink resolution needed for our use).
abspath() { case "$1" in /*) printf '%s\n' "$1" ;; *) printf '%s/%s\n' "$(pwd)" "$1" ;; esac; }

SRC_DIR="$ICONS_DIR/src"
REFRESH_SOURCES=0
case $# in
  0) ICON_SRC="$SRC_DIR/$ASSET_NAME.icon"; PNG_SRC="$SRC_DIR/$ASSET_NAME.png" ;;
  2) ICON_SRC="$(abspath "$1")"; PNG_SRC="$(abspath "$2")"; REFRESH_SOURCES=1 ;;
  *) die "usage: $(basename "$0") [<App.icon> <icon-1024.png>]   (no args = use repo sources in $SRC_DIR)" ;;
esac

[ -d "$ICON_SRC" ] || die ".icon not found (expected a folder bundle): $ICON_SRC"
[ -f "$PNG_SRC" ]  || die "PNG not found: $PNG_SRC"
command -v actool >/dev/null 2>&1 || die "actool not found — install Xcode (needed to compile .icon -> Assets.car)"

# Adopting a new icon: persist the external files as the repo sources, then
# generate from those canonical copies.
if [ "$REFRESH_SOURCES" = 1 ]; then
  mkdir -p "$SRC_DIR"
  rm -rf "$SRC_DIR/$ASSET_NAME.icon"
  cp -R "$ICON_SRC" "$SRC_DIR/$ASSET_NAME.icon"
  cp "$PNG_SRC" "$SRC_DIR/$ASSET_NAME.png"
  echo ">> refreshed repo sources in $SRC_DIR"
  ICON_SRC="$SRC_DIR/$ASSET_NAME.icon"
  PNG_SRC="$SRC_DIR/$ASSET_NAME.png"
fi

# Soft-check the PNG dimensions (tauri icon prefers a 1024x1024 source).
if command -v sips >/dev/null 2>&1; then
  w="$(sips -g pixelWidth  "$PNG_SRC" 2>/dev/null | awk '/pixelWidth/{print $2}')"
  h="$(sips -g pixelHeight "$PNG_SRC" 2>/dev/null | awk '/pixelHeight/{print $2}')"
  if [ "${w:-0}" != "1024" ] || [ "${h:-0}" != "1024" ]; then
    echo "WARN: PNG is ${w:-?}x${h:-?}, expected 1024x1024 (export at 1024 from Icon Composer)." >&2
  fi
fi

mkdir -p "$ICONS_DIR"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# 1) Liquid Glass: .icon -> Assets.car (asset always named "$ASSET_NAME").
cp -R "$ICON_SRC" "$TMP/$ASSET_NAME.icon"
mkdir -p "$TMP/car"
echo ">> actool: compiling $ASSET_NAME.icon -> Assets.car"
actool "$TMP/$ASSET_NAME.icon" --compile "$TMP/car" \
  --output-format human-readable-text --notices --warnings --errors \
  --output-partial-info-plist "$TMP/partial.plist" \
  --app-icon "$ASSET_NAME" --include-all-app-icons \
  --enable-on-demand-resources NO \
  --development-region en \
  --target-device mac \
  --minimum-deployment-target 26.0 \
  --platform macosx
[ -f "$TMP/car/Assets.car" ] || die "actool did not produce Assets.car"
cp "$TMP/car/Assets.car" "$ICONS_DIR/Assets.car"
echo ">> wrote $ICONS_DIR/Assets.car ($(wc -c < "$ICONS_DIR/Assets.car" | tr -d ' ') bytes)"

# 2) Flat fallback: 1024 PNG -> .icns/.ico/PNG via tauri icon.
echo ">> tauri icon: regenerating flat icon set from $(basename "$PNG_SRC")"
( cd "$GUI" && npx --no-install tauri icon "$PNG_SRC" )

# 3) Sanity-check the committed wiring (warn only — never edited here).
grep -q "CFBundleIconName" "$PLIST" 2>/dev/null \
  || echo "WARN: $PLIST missing CFBundleIconName=$ASSET_NAME — Liquid Glass won't apply on macOS 26+." >&2
grep -q "Assets.car" "$CONF" 2>/dev/null \
  || echo "WARN: $CONF bundle.macOS.files does not reference icons/Assets.car." >&2

echo ">> done. Verify: cd gui && npm run tauri build -- --debug --bundles app"
