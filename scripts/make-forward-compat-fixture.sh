#!/usr/bin/env bash
# make-forward-compat-fixture.sh — (re)generate the Phase 0 forward-compat
# fixture: a `.autosk/db` produced by the Go autosk binary (doltlite 0.10.8),
# plus the matching `tasks.golden` snapshot the Rust test asserts against.
#
# Usage:
#   make build            # build bin/autosk (Go, doltlite 0.10.8)
#   scripts/make-forward-compat-fixture.sh
#
# The task ids are random, so the db file and tasks.golden are a matched pair;
# this script always writes both together. Run from the repo root.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AUTOSK="$ROOT/bin/autosk"
DEST="$ROOT/crates/autosk-core/tests/fixtures/go-0.10.8"

if [ ! -x "$AUTOSK" ]; then
    echo "ERROR: $AUTOSK not found; run 'make build' first." >&2
    exit 1
fi

# Build the project in an isolated temp dir so we never touch the repo's own
# .autosk/db (and so AUTOSK_DB inherited from a running daemon can't interfere).
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"
unset AUTOSK_DB

"$AUTOSK" init --skip-bootstrap

"$AUTOSK" create "alpha task"   --priority 0 >/dev/null
"$AUTOSK" create "beta task"    --priority 1 -d "beta has a description" >/dev/null
"$AUTOSK" create "gamma task"   --priority 2 >/dev/null
"$AUTOSK" create "delta task"   --priority 3 >/dev/null
"$AUTOSK" create "epsilon task" --priority 2 >/dev/null

# Give the fixture a spread of terminal statuses (new/done/cancel).
delta="$("$AUTOSK" sql "SELECT id FROM tasks WHERE title='delta task'"   | tail -1)"
eps="$("$AUTOSK"   sql "SELECT id FROM tasks WHERE title='epsilon task'" | tail -1)"
"$AUTOSK" done   "$delta" >/dev/null
"$AUTOSK" cancel "$eps"   >/dev/null

mkdir -p "$DEST"
cp "$WORK/.autosk/db" "$DEST/db"

# Golden snapshot: id<TAB>title<TAB>status<TAB>priority, ordered by id. Strip the
# header row that `autosk sql` prints.
"$AUTOSK" --db "$DEST/db" sql \
    "SELECT id, title, status, priority FROM tasks ORDER BY id" \
    | tail -n +2 > "$DEST/tasks.golden"

echo "wrote $DEST/db ($(wc -c < "$DEST/db") bytes) and tasks.golden:"
cat "$DEST/tasks.golden"
