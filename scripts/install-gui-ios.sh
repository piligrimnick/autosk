#!/usr/bin/env bash
#
# install-gui-ios.sh — build the autosk GUI (Tauri) as a self-contained iOS app
# and install it on a USB-connected iPhone/iPad.
#
# The build is a RELEASE bundle: `tauri ios build` runs `beforeBuildCommand`
# (`npm run build`) and embeds the static frontend (`frontendDist: ../dist`), so
# the installed app has NO dependency on the Vite dev server. On device the app
# defaults to REMOTE mode (mobile cannot spawn autoskd) — point it at an autoskd
# running with `--tcp` from the in-app Settings (see "Next steps" printed at the
# end).
#
# ---------------------------------------------------------------------------
# USAGE
#   scripts/install-gui-ios.sh [options]
#
# OPTIONS
#   --device <id>            Target device identifier/name/UDID (as shown by
#                            `xcrun devicectl list devices`). Default: the single
#                            connected device is auto-detected; with more than one
#                            you must pass this (or set AUTOSK_IOS_DEVICE).
#   --export-method <m>      Xcode export method: debugging | release-testing |
#                            app-store-connect. Default: debugging
#                            (development-signed, the right choice for installing
#                            on your own device).
#   --target <triple>        Rust target to build (aarch64 | aarch64-sim |
#                            x86_64). Default: aarch64 (physical arm64 device).
#   --build-only             Build the IPA but do not install it.
#   --skip-build             Skip building; install the most recent existing IPA.
#   --open                   Open the generated Xcode project instead of building
#                            from the CLI (then Run on device from Xcode).
#   -h, --help               Show this help.
#
# ENVIRONMENT
#   AUTOSK_IOS_DEVICE        Same as --device.
#   APPLE_DEVELOPMENT_TEAM   Apple Team ID to sign with. Required the FIRST time
#                            the iOS project is generated (`tauri ios init`); on
#                            subsequent runs the team is read from the generated
#                            Xcode project. A free "Personal Team" works for
#                            development installs (the app expires after ~7 days).
#
# REQUIREMENTS
#   macOS + Xcode (xcodebuild, xcrun devicectl), Rust iOS targets, and the gui
#   workspace deps installed (`cd gui && npm install`). The script invokes the
#   local Tauri CLI (`gui/node_modules/.bin/tauri`, falling back to `npx tauri`).
# ---------------------------------------------------------------------------
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
gui="$repo_root/gui"
apple="$gui/src-tauri/gen/apple"

device="${AUTOSK_IOS_DEVICE:-}"
export_method="debugging"
target="aarch64"
build_only=0
skip_build=0
open_xcode=0

die() { echo "install-gui-ios: $*" >&2; exit 1; }

usage() { sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; s/^#$//; /^set -euo/d'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --device)        device="${2:?--device needs a value}"; shift 2;;
    --export-method) export_method="${2:?--export-method needs a value}"; shift 2;;
    --target)        target="${2:?--target needs a value}"; shift 2;;
    --build-only)    build_only=1; shift;;
    --skip-build)    skip_build=1; shift;;
    --open)          open_xcode=1; shift;;
    -h|--help)       usage; exit 0;;
    *) die "unknown option: $1 (see --help)";;
  esac
done

[ "$(uname -s)" = "Darwin" ] || die "iOS builds require macOS"
command -v xcrun >/dev/null 2>&1 || die "xcrun not found — install Xcode + command line tools"
[ -d "$gui" ] || die "gui/ not found at $gui"

# Resolve the Tauri CLI: prefer the workspace-local binary, fall back to npx.
tauri() {
  if [ -x "$gui/node_modules/.bin/tauri" ]; then
    (cd "$gui" && "./node_modules/.bin/tauri" "$@")
  else
    (cd "$gui" && npx --yes @tauri-apps/cli "$@")
  fi
}

# --open short-circuits the CLI build/install flow.
if [ "$open_xcode" -eq 1 ]; then
  [ -d "$apple" ] || tauri ios init
  "$repo_root/scripts/sync-gui-ios-icons.sh"
  echo "install-gui-ios: opening Xcode project — select your device and press Run"
  tauri ios open
  exit 0
fi

# 1. Build (unless --skip-build): generate the iOS project on first run, then
#    produce a signed, self-contained release IPA.
if [ "$skip_build" -eq 0 ]; then
  if [ ! -d "$apple" ]; then
    echo "install-gui-ios: iOS project not found — running 'tauri ios init'"
    [ -n "${APPLE_DEVELOPMENT_TEAM:-}" ] || \
      echo "install-gui-ios: WARNING: APPLE_DEVELOPMENT_TEAM is unset; 'tauri ios init' may prompt for a signing team" >&2
    tauri ios init
  fi
  # `tauri ios init` seeds the asset catalog with the stock Tauri logo and
  # ignores gui/src-tauri/icons/ios/, so sync the committed custom icon set into
  # the (gitignored, regenerated) catalog before every build — this also fixes
  # an existing gen/apple created before the custom icons were committed.
  "$repo_root/scripts/sync-gui-ios-icons.sh"
  echo "install-gui-ios: building release IPA (export-method=$export_method, target=$target)"
  tauri ios build --export-method "$export_method" --target "$target"
fi

# Stop here if the caller only wanted the artifact.
if [ "$build_only" -eq 1 ]; then
  ipa="$(find "$apple/build" -name '*.ipa' -type f -print0 2>/dev/null | xargs -0 ls -t 2>/dev/null | head -n1 || true)"
  [ -n "$ipa" ] || die "no .ipa produced under $apple/build"
  echo "install-gui-ios: built (no install requested): $ipa"
  exit 0
fi

# 2. Locate the newest IPA produced under the iOS build tree.
ipa="$(find "$apple/build" -name '*.ipa' -type f -print0 2>/dev/null | xargs -0 ls -t 2>/dev/null | head -n1 || true)"
[ -n "$ipa" ] || die "no .ipa found under $apple/build — build first (omit --skip-build)"

# 3. Resolve the target device. Auto-detect the single connected one when the
#    caller did not name it; refuse to guess when several are attached.
if [ -z "$device" ]; then
  # Portable (bash 3.2+) collection — no mapfile. Each line is one available
  # device's identifier; offline/unavailable rows are filtered out.
  connected=()
  while IFS= read -r id; do
    [ -n "$id" ] && connected+=("$id")
  done < <(
    xcrun devicectl list devices 2>/dev/null | awk '
      $0 ~ /available/ && $0 !~ /unavailable/ {
        for (i = 1; i <= NF; i++)
          if ($i ~ /^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$/) {
            print $i; break
          }
      }')
  case "${#connected[@]}" in
    0) die "no connected device found — plug in & unlock your iPad/iPhone, or pass --device";;
    1) device="${connected[0]}";;
    *) echo "install-gui-ios: multiple devices connected — pass --device <id>:" >&2
       xcrun devicectl list devices >&2
       exit 1;;
  esac
fi

# 4. Install over USB.
echo "install-gui-ios: installing $ipa -> device $device"
xcrun devicectl device install app --device "$device" "$ipa"

cat <<'EOF'

install-gui-ios: done.

Next steps (one-time, on the device & host):
  1. Trust the developer on the device (free/personal teams only):
       Settings > General > VPN & Device Management > trust your dev certificate.
  2. Run autoskd in TCP mode on this host so the device can reach it over Wi-Fi:
       autoskd serve --tcp 0.0.0.0:7777
     (a bare port binds 127.0.0.1 only — use 0.0.0.0 for LAN access).
  3. In the app's Settings (Remote mode is the default on mobile), enter:
       Host:  <this-host-LAN-IP>:7777     # e.g. `ipconfig getifaddr en0`
       Token: contents of ~/.autosk/daemon-token
     Allow the "Local Network" prompt on first connect. Both devices must be on
     the same network, and the host firewall must permit incoming connections.
EOF
