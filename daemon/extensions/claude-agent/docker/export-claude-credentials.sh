#!/usr/bin/env bash
# Bridge the host Claude Code OAuth credential into a file the Linux container's
# `claude` can read at $HOME/.claude/.credentials.json.
#
# macOS keeps the token in the LOGIN KEYCHAIN (item "Claude Code-credentials"),
# NOT in a file — so mounting ~/.claude alone carries no token. This exports the
# Keychain payload (already in the `{"claudeAiOauth":{…}}` shape Linux `claude`
# expects) to a 0600 file a Claude dockerSandbox workflow would overlay read-only
# at the container's ~/.claude/.credentials.json. On Linux it copies
# the existing ~/.claude/.credentials.json.
#
#   ./export-claude-credentials.sh           # write the default managed file
#   ./export-claude-credentials.sh /path     # write a custom destination
#   ./export-claude-credentials.sh --clean   # remove the managed file
#
# Run this BEFORE (re)starting the daemon — the workflow reads the file at load
# time. Re-run it whenever the token is refreshed (it has an expiry).
set -euo pipefail

SERVICE="Claude Code-credentials"
DEFAULT_DEST="${HOME}/.autosk/claude-runtime/.credentials.json"

if [[ "${1:-}" == "--clean" ]]; then
  rm -f "${2:-$DEFAULT_DEST}"
  echo "removed ${2:-$DEFAULT_DEST}"
  exit 0
fi

DEST="${1:-$DEFAULT_DEST}"

case "$(uname -s)" in
  Darwin)
    if ! json="$(security find-generic-password -s "$SERVICE" -w 2>/dev/null)"; then
      echo "error: no Keychain item '$SERVICE' found." >&2
      echo "       Log in to Claude Code on this Mac first (run 'claude' once), then re-run." >&2
      exit 1
    fi
    ;;
  *)
    src="${HOME}/.claude/.credentials.json"
    if [[ ! -f "$src" ]]; then
      echo "error: no $src on this host." >&2
      exit 1
    fi
    json="$(cat "$src")"
    ;;
esac

# Validate shape.
if ! printf '%s' "$json" | jq -e '.claudeAiOauth.accessToken' >/dev/null 2>&1; then
  echo "error: credential is not in the expected {\"claudeAiOauth\":{\"accessToken\":…}} shape." >&2
  exit 1
fi

# Warn (non-fatal) on an expired access token.
exp_ms="$(printf '%s' "$json" | jq -r '.claudeAiOauth.expiresAt // 0')"
now_ms="$(( $(date +%s) * 1000 ))"
if [[ "$exp_ms" =~ ^[0-9]+$ ]] && (( exp_ms > 0 )) && (( exp_ms < now_ms )); then
  echo "WARNING: the access token is EXPIRED. Run 'claude' once on the host to refresh," >&2
  echo "         then re-run this script (the container mounts it read-only, so it" >&2
  echo "         cannot refresh on its own)." >&2
fi

umask 077
mkdir -p "$(dirname "$DEST")"
printf '%s' "$json" > "$DEST"
chmod 600 "$DEST"
echo "wrote $DEST (0600)"
echo "mount this read-only at the container's ~/.claude/.credentials.json (the Claude dockerSandbox path)."
