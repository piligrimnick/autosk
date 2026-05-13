# autosk

A tiny task tracker for AI coding agents. CLI in the spirit of
[beads](https://github.com/steveyegge/beads), with a much smaller surface area
and a doltlite-backed local store (with a dolt-server backend planned).

```
$ autosk create "Wire up the auth flow" -p 1
as-a1b2

$ autosk ready --limit 1
ID       P  STATUS  TITLE
as-a1b2  1  new     Wire up the auth flow

$ autosk claim as-a1b2
id:          as-a1b2
status:      claimed
priority:    1
...
```

## Status

**v0.1.** Six acceptance scenarios green; full plan lives at
[`docs/plans/20260513-Init-Plan.md`](docs/plans/20260513-Init-Plan.md) and
[`docs/plans/20260513-Impl-Plan.md`](docs/plans/20260513-Impl-Plan.md).

## Install (from source)

```bash
# 1. Build doltlite (one-time):
cd ~/me/dev/doltlite && mkdir -p build && cd build
../configure && make doltlite-lib

# 2. Build autosk:
cd ~/me/dev/autosk
make build            # produces ./bin/autosk
```

The Makefile passes `DOLTLITE_DIR` through CGO so `mattn/go-sqlite3` links
against `libdoltlite.a`. Override `DOLTLITE_DIR` if your doltlite build lives
elsewhere.

## Quickstart

```bash
cd ~/your/project
autosk create "first task" -p 1     # auto-inits .autosk/db
autosk list                         # default: open work (new + claimed)
autosk ready --json                 # what should I work on?
autosk claim as-a1b2                # atomic, idempotent
autosk done as-a1b2
```

## Concepts

- **Task** — id, title, description, status, priority, timestamps.
- **Status** — one of `new`, `claimed`, `done`, `cancelled`. Any → any.
- **Priority** — `0..3`, `0` = highest.
- **Dependency** — directed `blocker → blocked` edge. The only kind in v0.1.
- **Ready set** — tasks where `status='new'` AND no open blocker
  (open = blocker status in `{new, claimed}`).
- **Blocked** — *derived*, not stored. A task is shown as `blocked: true` iff
  it has at least one open blocker.

## Command reference

```
Lifecycle
  autosk create [title] [-d desc | -d -] [-p N] [--blocks ID]... [--blocked-by ID]...
  autosk show <id>
  autosk update <id> [--title S] [--description S] [--status S] [--priority N]
  autosk claim <id>                   # idempotent: new|claimed → claimed
  autosk done <id>
  autosk cancel <id>
  autosk reopen <id>                  # done|cancelled → new

Blocking
  autosk block <id> <blocker-id>...
  autosk unblock <id> <blocker-id>... | --all
  autosk dep list <id>

Query
  autosk list [--status s,s | all] [--priority N] [--limit N]
  autosk ready [--limit N]
  autosk next                          # ready --limit 1

Admin
  autosk init [--prefix P]            # optional; writes auto-init
  autosk migrate
  autosk sql <query> [--write] [--pretty | --json]
  autosk version
  autosk history <id>                 # stub; v0.2 will use doltlite log
```

Every read command accepts `--json`. Every write command produces a
doltlite commit so future `autosk history` can recover field history.

## Environment

| Var | Effect |
|---|---|
| `AUTOSK_DB` | Override DB path (otherwise discovered by walking up). |
| `AUTOSK_NO_AUTOINIT` | Refuse to create a new DB on first write. |
| `DOLTLITE_DIR` | Build-time only: directory containing `libdoltlite.a` and `sqlite3.h`. |

## Roadmap (post v0.1)

Deferred per the [init plan §8](docs/plans/20260513-Init-Plan.md#8-explicitly-deferred-post-v01):

- doltserver backend for multi-writer collaboration
- comments
- pluggable workflows (status transition constraints)
- labels / tags
- full-text search
- audit log table / `autosk history` real implementation
- hooks / plugin events
- import/export / integrations

## License

MIT — see [LICENSE](LICENSE).
