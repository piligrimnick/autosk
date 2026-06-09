#!/usr/bin/env bash
#
# migrate-v11-to-v12.sh — migrate a Go/doltlite-0.10.8 `.autosk/db` (on-disk
# container format **v11**, `CTLD 0x0b`) into a fresh autoskd/doltlite-0.11.8
# database (container format **v12**, `CTLD 0x0c`).
#
# WHY THIS EXISTS
# ---------------
# doltlite 0.11.0 is a breaking on-disk format change (refs format v6 -> v7).
# The break is MUTUAL: a 0.10.8-written v11 DB is rejected by 0.11.8 with
# SQLITE_NOTADB, and vice-versa (pinned by crates/autosk-core/tests/
# format_compat.rs). autoskd/GUI link 0.11.8, so they physically cannot open
# the old v11 `.autosk/db` — there is no in-place upgrade. The plan declares
# autoskd greenfield and v11->v12 migration out of scope
# (docs/plans/20260607-Rust-Daemon-Tauri-GUI.md §2.1).
#
# The LOGICAL SQL schema is identical across the split (the Rust migrator
# reproduces Go's 001_init.sql verbatim), so migration is a per-row export
# (read with a 0.10.8 engine) + import (write with a 0.11.8 engine). The dolt
# COMMIT history (versioning) is NOT preserved — only the current rows are.
#
# HOW IT WORKS
# ------------
# Because the two doltlite versions export the same sqlite3 C symbols, they
# cannot be linked into one binary; the bridge is therefore two pinned builds,
# checked out at exact commits in throwaway git worktrees:
#
#   READER  main @ <READER_COMMIT>     -> Go CLI linked against doltlite 0.10.8
#                                         (opens the v11 DB directly via --db)
#   WRITER  feat/gui @ <WRITER_COMMIT> -> autoskd linked against doltlite 0.11.8
#                                         (+ CGO-free CLI; writes the fresh v12)
#
# Tables are copied in FK-safe order as chunked multi-row INSERTs, and session
# transcript files (.autosk/sessions, plain files — format-agnostic) are copied
# verbatim. Row counts are verified at the end.
#
# USAGE
# -----
#   scripts/migrate-v11-to-v12.sh [--src <v11-db>] [--out <dir>] [--work <dir>]
#                                 [--adopt] [--keep-work]
#
#   --src   <path>  source v11 db            (default: <repo>/.autosk/db)
#   --out   <dir>   destination PROJECT dir for the fresh v12 db; MUST be
#                   outside any tree containing a .autosk/db, otherwise project
#                   discovery walks up and finds the parent's v11 db
#                   (default: ${TMPDIR}/autosk-migrate/out)
#   --work  <dir>   scratch dir for the pinned worktrees + builds
#                   (default: ${TMPDIR}/autosk-migrate/work)
#   --adopt         after a verified migration, back up <repo>/.autosk/db and
#                   swap the migrated v12 db (+ sessions) into place
#   --keep-work     do not delete the worktrees/builds on exit (faster re-runs)
#
set -euo pipefail

# --- pinned commits ----------------------------------------------------------
# READER: main / release 0.1.6 — DOLTLITE_VERSION 0.10.8, Go links doltlite via
#         CGO and opens .autosk/db directly (pre-RPC).
READER_COMMIT="5054b85aff7e16c31ea4e64e5c29cc5e9d65d129"
READER_DOLTLITE="0.10.8"
# WRITER: feat/gui — autoskd links doltlite 0.11.8 (writes v12); the Go CLI is
#         CGO-free and a pure JSON-RPC client of that daemon.
WRITER_COMMIT="613ece155fa9273cfd5562ad105a9252baad07b5"
WRITER_DOLTLITE="0.11.8"

# --- locate the repo (script lives at <repo>/scripts/) -----------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)"

# --- defaults / arg parsing --------------------------------------------------
BASE="${TMPDIR:-/tmp}/autosk-migrate"
SRC="$REPO/.autosk/db"
OUT="$BASE/out"
WORK="$BASE/work"
ADOPT=0
KEEP_WORK=0

while [ $# -gt 0 ]; do
  case "$1" in
    --src)  SRC="$2"; shift 2;;
    --out)  OUT="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --adopt) ADOPT=1; shift;;
    --keep-work) KEEP_WORK=1; shift;;
    -h|--help) sed -n '2,60p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

SRC="$(cd "$(dirname "$SRC")" && pwd)/$(basename "$SRC")"  # absolutize
OUT_DB="$OUT/.autosk/db"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# --- preflight: source must be a v11 doltlite container ----------------------
[ -f "$SRC" ] || die "source db not found: $SRC"
HDR="$(head -c 5 "$SRC" | od -An -tx1 | tr -d ' \n')"
case "$HDR" in
  43544c440b) : ;;                                  # "CTLD" + 0x0b = v11
  43544c440c) die "source is already container v12 ($SRC) — nothing to migrate";;
  43544c44*)  die "source is doltlite but an unexpected container version (header=$HDR)";;
  *)          die "source is not a doltlite CTLD container (header=$HDR): $SRC";;
esac
log "source confirmed v11 (CTLD 0x0b): $SRC"

# guard: OUT must not sit under a tree that already has a .autosk/db, or
# project discovery during `init` will resolve the ancestor's v11 db.
probe="$(dirname "$OUT")"
while [ "$probe" != "/" ]; do
  if [ -e "$probe/.autosk/db" ] && [ "$probe/.autosk/db" != "$OUT_DB" ]; then
    die "--out ($OUT) is inside a project with a .autosk/db at $probe — pick a path outside any autosk project"
  fi
  probe="$(dirname "$probe")"
done

# --- pinned doltlite caches (reuse the repo's, else builds will fetch) --------
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64)  PLAT="osx-arm64";;
  Darwin/x86_64) PLAT="osx-x64";;
  Linux/x86_64)  PLAT="linux-x64";;
  Linux/aarch64) PLAT="linux-arm64";;
  *)             PLAT="";;
esac
READER_DOLT_DIR="$REPO/.doltlite/${READER_DOLTLITE}-${PLAT}"
WRITER_DOLT_DIR="$REPO/.doltlite/${WRITER_DOLTLITE}-${PLAT}"

# --- private daemon socket so we never touch the operator's running autoskd --
export AUTOSK_SOCK="$WORK/daemon.sock"
export AUTOSK_IDLE_SECS=0   # disable idle-shutdown for our pinned daemon
DAEMON_PID=""

stop_daemon() {
  if [ -n "${DAEMON_PID:-}" ]; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    DAEMON_PID=""
  fi
  pkill -f "autoskd serve --sock $AUTOSK_SOCK" 2>/dev/null || true
  rm -f "$AUTOSK_SOCK" 2>/dev/null || true
}

cleanup() {
  stop_daemon
  if [ "$KEEP_WORK" -eq 0 ]; then
    git -C "$REPO" worktree remove --force "$WORK/reader" 2>/dev/null || true
    git -C "$REPO" worktree remove --force "$WORK/writer" 2>/dev/null || true
    rm -rf "$WORK" 2>/dev/null || true
  fi
}
trap cleanup EXIT

mkdir -p "$WORK"

# --- build the two pinned bridges -------------------------------------------
ensure_worktree() {
  local dir="$1" commit="$2"
  git -C "$REPO" cat-file -e "${commit}^{commit}" 2>/dev/null \
    || die "commit $commit not present locally — fetch it first"
  if [ ! -e "$dir/.git" ]; then
    log "checking out $commit -> $dir"
    git -C "$REPO" worktree add --force --detach "$dir" "$commit" >/dev/null
  fi
}

build_make() {  # <worktree> <target> <doltlite-dir-or-empty>
  local dir="$1" target="$2" dolt="$3"
  if [ -n "$dolt" ] && [ -d "$dolt" ]; then
    DOLTLITE_DIR="$dolt" make -C "$dir" "$target"
  else
    warn "doltlite cache $dolt missing — build will fetch from the network"
    make -C "$dir" "$target"
  fi
}

log "building READER (main @ ${READER_COMMIT:0:7}, doltlite $READER_DOLTLITE)"
ensure_worktree "$WORK/reader" "$READER_COMMIT"
READER_BIN="$WORK/reader/bin/autosk"
[ -x "$READER_BIN" ] || build_make "$WORK/reader" build "$READER_DOLT_DIR"
[ -x "$READER_BIN" ] || die "reader build failed: $READER_BIN missing"

log "building WRITER (feat/gui @ ${WRITER_COMMIT:0:7}, autoskd doltlite $WRITER_DOLTLITE)"
ensure_worktree "$WORK/writer" "$WRITER_COMMIT"
WRITER_BIN="$WORK/writer/bin/autosk"
WRITER_DAEMON="$WORK/writer/target/debug/autoskd"
[ -x "$WRITER_BIN" ]    || build_make "$WORK/writer" build ""
[ -x "$WRITER_DAEMON" ] || build_make "$WORK/writer" build-autoskd "$WRITER_DOLT_DIR"
[ -x "$WRITER_BIN" ]    || die "writer CLI build failed: $WRITER_BIN missing"
[ -x "$WRITER_DAEMON" ] || die "writer daemon build failed: $WRITER_DAEMON missing"

# Start ONE long-lived pinned daemon and keep it for the WHOLE migration.
# An auto-spawned daemon is a child of the `autosk` CLI that triggered it and
# can exit when that CLI exits; the next call then respawns a fresh daemon,
# which re-runs migrate() and RE-SEEDS the `human` agent (migrate.rs always
# re-seeds it when absent). Mid-import that collides on agents.name. A single
# persistent daemon opens the project — and seeds `human` — exactly once.
export AUTOSKD_BIN="$WRITER_DAEMON"
log "starting pinned autoskd (doltlite $WRITER_DOLTLITE) on $AUTOSK_SOCK"
nohup "$WRITER_DAEMON" serve --sock "$AUTOSK_SOCK" >"$WORK/autoskd.log" 2>&1 &
DAEMON_PID=$!
for _ in $(seq 1 100); do [ -S "$AUTOSK_SOCK" ] && break; sleep 0.1; done
[ -S "$AUTOSK_SOCK" ] || die "pinned daemon did not create $AUTOSK_SOCK (see $WORK/autoskd.log)"
kill -0 "$DAEMON_PID" 2>/dev/null || die "pinned daemon exited at startup (see $WORK/autoskd.log)"

# --- create the fresh v12 db -------------------------------------------------
log "initialising fresh v12 db at $OUT_DB"
rm -rf "$OUT"
mkdir -p "$OUT"
( cd "$OUT" && "$WRITER_BIN" init --skip-bootstrap >/dev/null )
[ -f "$OUT_DB" ] || die "init did not create $OUT_DB"
NEWHDR="$(head -c 5 "$OUT_DB" | od -An -tx1 | tr -d ' \n')"
[ "${NEWHDR:0:10}" = "43544c440c" ] || warn "fresh db header is $NEWHDR (expected CTLD 0x0c / v12)"

# NB: we deliberately do NOT clear the row `init` seeds (the canonical `human`
# agent). doltlite has a phantom-unique-index bug — DELETE-ing a row leaves a
# ghost entry in its UNIQUE(name) index, so a later INSERT of the same name
# fails `UNIQUE constraint failed` even though the table reads empty. The Python
# importer therefore KEEPS the seeded human, skips the v11 `human` row, and
# remaps the v11 human id onto the seeded one in every agent reference. Every
# other target table starts empty, so plain INSERTs never hit the bug.

# --- copy every row, table by table, in FK-safe order ------------------------
log "copying rows v11 -> v12"
READER_BIN="$READER_BIN" WRITER_BIN="$WRITER_BIN" SRC_DB="$SRC" OUT_DB="$OUT_DB" \
  python3 - <<'PYEOF'
import json, os, subprocess, sys

READER = os.environ["READER_BIN"]
WRITER = os.environ["WRITER_BIN"]
SRC    = os.environ["SRC_DB"]
OUTDB  = os.environ["OUT_DB"]

# (table, columns) in FK-safe INSERT order. Explicit column lists keep us
# independent of SELECT * ordering and pin the exact shape we write.
TABLES = [
    ("agents",           ["id", "name", "is_human", "created_at"]),
    ("workflows",        ["id", "name", "description", "first_step_id",
                          "is_synthetic", "isolation", "created_at"]),
    ("steps",            ["id", "workflow_id", "name", "agent_id", "seq",
                          "agent_params", "max_visits"]),
    ("step_transitions", ["id", "step_id", "next_step_id", "task_status",
                          "prompt_rule"]),
    ("tasks",            ["id", "title", "description", "status", "priority",
                          "author_id", "workflow_id", "current_step_id",
                          "metadata", "created_at", "updated_at"]),
    ("task_deps",        ["blocker_id", "blocked_id", "kind"]),
    ("comments",         ["id", "task_id", "author_id", "text", "created_at"]),
    ("daemon_runs",      ["job_id", "task_id", "step_id", "status",
                          "transition_id", "exit_code", "pid", "pi_session_id",
                          "session_path", "error", "corrections_used",
                          "max_corrections", "created_at", "started_at",
                          "finished_at"]),
    ("step_signals",     ["run_id", "task_id", "transition_id", "created_at"]),
]

CHUNK_ROWS  = 200
CHUNK_BYTES = 256 * 1024


def reader_json(query):
    p = subprocess.run([READER, "--db", SRC, "--json", "sql", query],
                       capture_output=True, text=True)
    if p.returncode != 0:
        sys.exit("READER failed (%s):\n%s" % (query, p.stderr.strip()))
    return json.loads(p.stdout or "[]") or []


def writer_exec(sql):
    p = subprocess.run([WRITER, "--db", OUTDB, "sql", "--write", "--file", "-"],
                       input=sql, capture_output=True, text=True)
    if p.returncode != 0:
        sys.exit("WRITER failed:\n%s\nSQL head: %s" % (p.stderr.strip(), sql[:300]))


def writer_count(table):
    p = subprocess.run([WRITER, "--db", OUTDB, "--json", "sql",
                        "SELECT count(*) AS n FROM %s" % table],
                       capture_output=True, text=True)
    rows = json.loads(p.stdout or "[]")
    return int(rows[0]["n"]) if rows else 0


def lit(v):
    if v is None:
        return "NULL"
    if isinstance(v, bool):
        return "1" if v else "0"
    if isinstance(v, int):
        return str(v)
    if isinstance(v, float):
        # all numeric columns are INTEGER; render plainly if a float slips in
        return repr(int(v)) if v.is_integer() else repr(v)
    return "'" + str(v).replace("'", "''") + "'"   # SQLite literal escaping


def writer_one_id(query):
    p = subprocess.run([WRITER, "--db", OUTDB, "--json", "sql", query],
                       capture_output=True, text=True)
    rows = json.loads(p.stdout or "[]")
    return rows[0]["id"] if rows else None


# The fresh v12 db pre-seeds exactly one row: the canonical `human` agent, with
# a DIFFERENT random id than the v11 db. Reuse it instead of importing the v11
# `human` row (which would trip doltlite's phantom-unique-index bug), and remap
# the v11 human id onto the seeded one wherever an agents.id FK points at it.
old_rows = reader_json("SELECT id FROM agents WHERE name='human'")
OLD_HUMAN = old_rows[0]["id"] if old_rows else None
NEW_HUMAN = writer_one_id("SELECT id FROM agents WHERE name='human'")
remap = {}
if OLD_HUMAN and NEW_HUMAN and OLD_HUMAN != NEW_HUMAN:
    remap[OLD_HUMAN] = NEW_HUMAN
    print("  human agent: reusing pre-seeded %s (v11 %s remapped)" % (NEW_HUMAN, OLD_HUMAN))

# columns holding an agents.id FK that the human remap must rewrite
AGENT_REF_COLS = {"tasks": {"author_id"}, "comments": {"author_id"}, "steps": {"agent_id"}}

src_counts = {}
for table, cols in TABLES:
    rows = reader_json("SELECT %s FROM %s" % (",".join(cols), table))
    src_counts[table] = len(rows)             # full v11 count (incl. human)
    if table == "agents":
        rows = [r for r in rows if r.get("name") != "human"]   # keep pre-seeded human
    if not rows:
        print("  %-16s %6d rows" % (table, 0))
        continue
    refcols = AGENT_REF_COLS.get(table, set())

    def cell(r, c, refcols=refcols):
        v = r.get(c)
        if c in refcols and v in remap:
            v = remap[v]
        return lit(v)

    prefix = "INSERT INTO %s (%s) VALUES " % (table, ",".join(cols))
    tuples = ["(" + ",".join(cell(r, c) for c in cols) + ")" for r in rows]
    i, written = 0, 0
    while i < len(tuples):
        chunk, clen = [], 0
        while i < len(tuples) and len(chunk) < CHUNK_ROWS and clen < CHUNK_BYTES:
            chunk.append(tuples[i]); clen += len(tuples[i]); i += 1
        writer_exec(prefix + ",".join(chunk) + ";")
        written += len(chunk)
    print("  %-16s %6d rows" % (table, written))

print("verifying row counts (v11 source vs v12 dest):")
ok = True
for table, _ in TABLES:
    s = src_counts[table]
    d = writer_count(table)
    flag = "OK" if s == d else "MISMATCH"
    if s != d:
        ok = False
    print("  %-16s src=%6d  dst=%6d  %s" % (table, s, d, flag))

sys.exit(0 if ok else 3)
PYEOF

# row copy done; stop the daemon so the db file is closed + flushed before we
# copy/adopt it (working-set writes are durable per-statement, but a clean stop
# avoids copying an actively-open doltlite file).
stop_daemon

# --- copy session transcripts (plain files; format-agnostic) -----------------
SRC_SESS="$(dirname "$SRC")/sessions"
if [ -d "$SRC_SESS" ]; then
  log "copying session transcripts"
  rm -rf "$OUT/.autosk/sessions"
  cp -R "$SRC_SESS" "$OUT/.autosk/sessions"
fi

log "migration complete -> $OUT_DB"
echo
echo "  Migrated v12 project: $OUT"
echo "  (dolt commit history is NOT carried over — only current rows.)"
echo

# --- optional in-place adoption ---------------------------------------------
if [ "$ADOPT" -eq 1 ]; then
  TS="$(date +%Y%m%d-%H%M%S)"
  BACKUP="$SRC.v11.bak.$TS"
  log "adopting: backing up $SRC -> $BACKUP and swapping in the v12 db"
  cp "$SRC" "$BACKUP"
  cp "$OUT_DB" "$SRC"
  if [ -d "$OUT/.autosk/sessions" ]; then
    rm -rf "$(dirname "$SRC")/sessions"
    cp -R "$OUT/.autosk/sessions" "$(dirname "$SRC")/sessions"
  fi
  echo "  adopted. v11 backup kept at: $BACKUP"
else
  echo "  To adopt it into $(dirname "$SRC") (the GUI/daemon will then open it):"
  echo "    cp \"$SRC\" \"$SRC.v11.bak\""
  echo "    cp \"$OUT_DB\" \"$SRC\""
  echo "    cp -R \"$OUT/.autosk/sessions\" \"$(dirname "$SRC")/sessions\"  # if present"
  echo
  echo "  …or re-run with --adopt to do the backup + swap automatically."
fi
