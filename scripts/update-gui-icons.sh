#!/usr/bin/env bash
# update-gui-icons.sh — regenerate the autosk GUI (Tauri) app icons from an
# Apple Icon Composer ".icon" file plus two 1024x1024 PNG exports (desktop +
# iOS).
#
# ---------------------------------------------------------------------------
# USAGE
#   # regenerate from the sources committed in the repo (the common case):
#   scripts/update-gui-icons.sh
#
#   # adopt a NEW icon from external files (also refreshes the stored sources
#   # under gui/src-tauri/icons/src/ so the repo copies stay authoritative).
#   # The 3rd (iOS) PNG is optional — omit it to keep the committed iOS source:
#   scripts/update-gui-icons.sh <App.icon> <icon-1024.png> [<icon-ios-1024.png>]
#
# EXAMPLE
#   scripts/update-gui-icons.sh ~/Downloads/autosk.icon \
#     ~/Downloads/autosk.png ~/Downloads/autosk-iOS-Default-1024x1024@1x.png
#
# Run from anywhere; paths may be relative or absolute. The script writes into
# gui/src-tauri/icons/ inside this repo.
#
# SOURCES OF TRUTH (committed; stored in Git LFS)
#   gui/src-tauri/icons/src/autosk.icon     — Icon Composer bundle (Liquid Glass)
#   gui/src-tauri/icons/src/autosk.png      — 1024x1024 flat export (macOS look)
#   gui/src-tauri/icons/src/autosk-ios.png  — 1024x1024 iOS export (full-bleed)
#   With no args the script regenerates from these. With args it copies the
#   given files over them first. (Binary icon artifacts under icons/ are tracked
#   via gui/src-tauri/icons/.gitattributes — contributors need `git lfs install`.)
# ---------------------------------------------------------------------------
# WHY THREE INPUTS?
#   A ".icon" is an Icon Composer *bundle* (a folder of vector layers + a
#   icon.json describing the Liquid Glass treatment) — it is NOT a ready image.
#   We need it in three different shapes:
#
#     1. Assets.car  — the compiled Liquid Glass icon for macOS 26+. Produced
#        from the .icon by `actool`.
#     2. A flat .icns/.ico/PNG set — the cross-platform fallback (macOS < 26,
#        Windows, Linux). Produced by `tauri icon` from the macOS 1024 PNG.
#     3. The iOS app-icon set (gui/src-tauri/icons/ios/*.png). iOS rejects ANY
#        alpha channel on the large icon (altool error 90717) and wants a
#        FULL-BLEED, opaque square (it applies its own corner mask), so the
#        squircle-masked macOS export is unusable here. Instead we take the
#        Icon Composer iOS export and flatten it to opaque via
#        scripts/flatten-ios-icon.swift, then downscale to every iOS size.
#
#   The macOS flat set needs a crisp, properly-masked 1024 image. The standalone
#   .icns that actool emits caps at 256px, and the raw layer art inside the
#   .icon has opaque (white) corners — neither is usable directly. So you must
#   also export a 1024x1024 PNG from Icon Composer (it applies the squircle
#   mask, transparency and margins for you).
#
# HOW TO PRODUCE THE INPUTS (Apple Icon Composer)
#   1. Design + save the icon          -> gives you "App.icon".
#   2. File -> Export -> macOS PNG 1024 -> gives you the flat fallback PNG.
#   3. File -> Export -> iOS PNG 1024   -> gives you the full-bleed iOS PNG.
# ---------------------------------------------------------------------------
# WHAT THIS SCRIPT DOES
#   1. Copies the input .icon to a temp "autosk.icon" so the compiled asset is
#      ALWAYS named "autosk" (regardless of the input filename), matching
#      CFBundleIconName in gui/src-tauri/Info.plist.
#   2. actool compiles it -> gui/src-tauri/icons/Assets.car (Liquid Glass).
#   3. `tauri icon <png>` regenerates the flat .icns/.ico/PNG set in
#      gui/src-tauri/icons/.
#   4. Flattens the iOS export to an opaque master and rewrites
#      gui/src-tauri/icons/ios/*.png (full-bleed, no alpha) at every size.
#   5. Verifies the (already committed) wiring is still in place and warns if
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
#   - macOS with Xcode installed: `actool` (compile the .icon) and `swift` (run
#     flatten-ios-icon.swift) both ship with Xcode. Once the assets are
#     committed, building the app on a machine WITHOUT Xcode (e.g. CI) works fine
#     — CI just consumes the committed icons/ios/*.png.
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
IOS_PNG_SRC="$SRC_DIR/$ASSET_NAME-ios.png"   # committed iOS source (full-bleed)
REFRESH_SOURCES=0
REFRESH_IOS=0
case $# in
  0) ICON_SRC="$SRC_DIR/$ASSET_NAME.icon"; PNG_SRC="$SRC_DIR/$ASSET_NAME.png" ;;
  2) ICON_SRC="$(abspath "$1")"; PNG_SRC="$(abspath "$2")"; REFRESH_SOURCES=1 ;;
  3) ICON_SRC="$(abspath "$1")"; PNG_SRC="$(abspath "$2")"; IOS_PNG_SRC="$(abspath "$3")"; REFRESH_SOURCES=1; REFRESH_IOS=1 ;;
  *) die "usage: $(basename "$0") [<App.icon> <icon-1024.png> [<icon-ios-1024.png>]]   (no args = use repo sources in $SRC_DIR)" ;;
esac

[ -d "$ICON_SRC" ] || die ".icon not found (expected a folder bundle): $ICON_SRC"
[ -f "$PNG_SRC" ]  || die "PNG not found: $PNG_SRC"
[ -f "$IOS_PNG_SRC" ] || die "iOS PNG not found: $IOS_PNG_SRC (export an iOS 1024 PNG from Icon Composer; pass it as the 3rd arg to adopt)"
command -v actool >/dev/null 2>&1 || die "actool not found — install Xcode (needed to compile .icon -> Assets.car)"
command -v swift  >/dev/null 2>&1 || die "swift not found — install Xcode (needed to flatten the iOS icon)"

# Adopting a new icon: persist the external files as the repo sources, then
# generate from those canonical copies.
if [ "$REFRESH_SOURCES" = 1 ]; then
  mkdir -p "$SRC_DIR"
  rm -rf "$SRC_DIR/$ASSET_NAME.icon"
  cp -R "$ICON_SRC" "$SRC_DIR/$ASSET_NAME.icon"
  cp "$PNG_SRC" "$SRC_DIR/$ASSET_NAME.png"
  [ "$REFRESH_IOS" = 1 ] && cp "$IOS_PNG_SRC" "$SRC_DIR/$ASSET_NAME-ios.png"
  echo ">> refreshed repo sources in $SRC_DIR"
  ICON_SRC="$SRC_DIR/$ASSET_NAME.icon"
  PNG_SRC="$SRC_DIR/$ASSET_NAME.png"
  IOS_PNG_SRC="$SRC_DIR/$ASSET_NAME-ios.png"
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

# 2) Flat fallback: 1024 PNG -> .icns/.ico/PNG via tauri icon. This also emits a
#    gui/src-tauri/icons/ios/ set, but from the squircle-masked macOS PNG (it has
#    transparent corners) — step 3 overwrites that set with an opaque one.
echo ">> tauri icon: regenerating flat icon set from $(basename "$PNG_SRC")"
( cd "$GUI" && npx --no-install tauri icon "$PNG_SRC" )

# 3) iOS app icons MUST be full-bleed + opaque (Apple rejects any alpha on the
#    large icon, error 90717). Flatten the Icon Composer iOS export to an opaque
#    1024 master, then downscale to every size tauri just created (keeping the
#    exact filename set the Xcode AppIcon.appiconset expects). iOS applies its
#    own corner mask on device.
IOS_DIR="$ICONS_DIR/ios"
if [ -d "$IOS_DIR" ]; then
  IOS_MASTER="$TMP/ios-master-1024.png"
  echo ">> flatten-ios-icon: $(basename "$IOS_PNG_SRC") -> opaque 1024 master"
  swift "$ROOT/scripts/flatten-ios-icon.swift" "$IOS_PNG_SRC" "$IOS_MASTER" 1024
  # Derive the pixel size from each AppIcon filename: <base>@<scale>x.png, where
  # <base> is "NxN" or "N" (e.g. 83.5x83.5@2x -> 167, 512@2x -> 1024).
  ios_px() {
    awk -v n="$1" 'BEGIN{
      sub(/^AppIcon-/,"",n); sub(/\.png$/,"",n);
      split(n,a,"@"); base=a[1]; scl=a[2]; sub(/x.*/,"",base); sub(/x$/,"",scl);
      printf "%.0f", base*scl
    }'
  }
  count=0
  for f in "$IOS_DIR"/*.png; do
    [ -e "$f" ] || continue
    px="$(ios_px "$(basename "$f")")"
    sips -z "$px" "$px" "$IOS_MASTER" --out "$f" >/dev/null 2>&1 \
      || die "sips failed to render $(basename "$f") at ${px}px"
    count=$((count + 1))
  done
  echo ">> wrote $count opaque iOS app icons to $IOS_DIR"
else
  echo "WARN: $IOS_DIR not found — skipped iOS app-icon regeneration." >&2
fi

# 4) Sanity-check the committed wiring (warn only — never edited here).
grep -q "CFBundleIconName" "$PLIST" 2>/dev/null \
  || echo "WARN: $PLIST missing CFBundleIconName=$ASSET_NAME — Liquid Glass won't apply on macOS 26+." >&2
grep -q "Assets.car" "$CONF" 2>/dev/null \
  || echo "WARN: $CONF bundle.macOS.files does not reference icons/Assets.car." >&2

echo ">> done. Verify: cd gui && npm run tauri build -- --debug --bundles app"
