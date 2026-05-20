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

**v0.2.** Tasks are now first-class citizens of a small workflow engine:
agents, workflows, comments, and a daemon poller that drives tasks
through step transitions. An interactive TUI (`autosk lazy`) renders
all four entity kinds plus an SSE-backed job inspector. See:

- Workflows plan: [`docs/plans/20260517-Workflows-Plan.md`](docs/plans/20260517-Workflows-Plan.md).
- Agent packages plan: [`docs/plans/20260518-Agent-Packages.md`](docs/plans/20260518-Agent-Packages.md).
- Concept doc + walkthrough: [`docs/workflows.md`](docs/workflows.md).
- Daemon details: [`docs/daemon.md`](docs/daemon.md).
- Interactive TUI (`autosk lazy`): [`docs/lazy.md`](docs/lazy.md).

There is **no migration** from v0.1: opening a v0.1 database with v0.2
binary refuses with `schema_v1_unsupported`. Wipe `.autosk/db` and
re-init.

**Upgrade note (from a pre-2026-05-18 v0.2 checkout):** the file-based
`.autosk/agents/<name>.toml` mechanism is gone. Agents now come from
npm packages installed via `autosk agent install <pkg>`. After
upgrading, install replacement packages for each agent your workflows
reference and (optionally) `rm -rf .autosk/agents/` — the directory is
ignored, not parsed. See
[`docs/plans/20260518-Agent-Packages.md`](docs/plans/20260518-Agent-Packages.md).

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

- **Task** — id, title, description, status, priority, optional FKs to
  `author_id` / `workflow_id` / `current_step_id`, timestamps.
- **Status** — one of `new`, `in_workflow`, `human_feedback`, `done`,
  `cancelled`. A SQL CHECK ties `status='in_workflow'` to
  `current_step_id IS NOT NULL`.
- **Priority** — `0..3`, `0` = highest.
- **Agent** — a named actor that can own a task. `human` is seeded on
  init; background agents are npm packages installed into
  `~/.autosk/packages/` (see `autosk agent install`).
- **Workflow** — a directed graph of `steps`; each step has an agent and
  ≥1 outgoing transition. The daemon advances tasks via step transitions.
- **Dependency** — directed `blocker → blocked` edge.
- **Ready set** — tasks where `status='new'` AND no open blocker (open =
  blocker status in `{new, in_workflow, human_feedback}`).
- **Blocked** — *derived*, not stored.

## Command reference

```
Lifecycle
  autosk create [title] [-d desc | -d -] [-p N]
               [--workflow NAME | --agent NAME]
               [--blocks ID]... [--blocked-by ID]...
  autosk show <id>
  autosk update <id> [--title S] [--description S] [--status S] [--priority N]
  autosk assign <id> --agent NAME    # only valid on status=new
  autosk enroll <id> --workflow NAME # attach an existing `new` task to a workflow
  autosk enroll <id> --agent    NAME # ...or the synthetic single:<NAME> flow
  autosk resume <id> [--to STEP]     # human_feedback → in_workflow
  autosk done <id>                   # direct; also clears current_step_id
  autosk cancel <id>                 # direct; also clears current_step_id
  autosk reopen <id>                 # done|cancelled → new (preserves workflow_id)

Agents (npm-package-based)
  autosk agent install <npm-name> [--version SPEC]
  autosk agent uninstall <npm-name> [--force]
  autosk agent list                  # union of installed pkgs + DB rows
  autosk agent show <npm-name>
  autosk agent runtime install       # eager install @autosk/agent-runtime

Workflows
  autosk workflow create --file PATH
  autosk workflow list [--all]       # --all shows synthetic single:* workflows
  autosk workflow show <name>
  autosk workflow delete <name>

Agent-facing (inside a workflow step)
  autosk step next <id> --to <step-or-status>

Comments
  autosk comment add <id> [text]     # text from stdin if omitted or '-'
  autosk comment list <id>

Blocking
  autosk block <id> <blocker-id>...
  autosk unblock <id> <blocker-id>... | --all
  autosk dep list <id>

Query
  autosk list [--status s,s | all] [--priority N] [--limit N]
  autosk ready [--limit N]
  autosk next                          # ready --limit 1

Admin
  autosk init [--prefix P]
  autosk migrate
  autosk sql <query> [--write] [--pretty | --json]
  autosk version
  autosk history <id>                  # stub

Daemon
  autosk daemon serve [--sock PATH] [--workers N] [--poll-interval 2s] ...
  autosk daemon list [--all-projects]
  autosk daemon status <job-id> / messages <job-id> / cancel <job-id>

Interactive TUI
  autosk lazy [--job ID] [--sock PATH] [--refresh 2s]
```

Every read command accepts `--json`. Every write command produces a
doltlite commit so future `autosk history` can recover field history.

## Environment

| Var | Effect |
|---|---|
| `AUTOSK_DB` | Override DB path (otherwise discovered by walking up). |
| `AUTOSK_NO_AUTOINIT` | Refuse to create a new DB on first write. |
| `AUTOSK_AGENT` | Name of the agent the CLI is running as (default `human`). Used to fill `tasks.author_id` and `comments.author_id`. |
| `AUTOSK_PACKAGES` | Override the global agent-packages prefix (defaults to `~/.autosk/packages` / `$XDG_DATA_HOME/autosk/packages`). |
| `AUTOSK_BIN` | Used by `@autosk/agent-runtime` so custom-runner agents can shell the right autosk binary. Defaults to `autosk` on PATH. |
| `DOLTLITE_DIR` | Build-time only: directory containing `libdoltlite.a` and `sqlite3.h`. |

## Daemon / pi orchestrator

`autosk daemon serve` is a per-host service that listens on a unix-
domain socket and runs autosk workflow steps. **One process per host
serves any number of projects**; the project for each request is
selected by the `X-Autosk-Cwd` header that the `autosk daemon ...`
client commands attach for you.

```bash
# 1. Start the daemon (defaults to ~/.autosk/daemon.sock, workers=2).
autosk daemon serve &

# 2. From any project root, enroll a task into a workflow. The per-
#    project poller picks it up automatically; no submit step.
id=$(autosk create "do thing" -p 1 --json | jq -r .id)
autosk enroll "$id" --workflow feature-dev

# 3. Inspect.
autosk daemon list                  # this project only
autosk daemon list --all-projects   # every loaded project
autosk daemon status   <job-id>
autosk daemon messages <job-id> --limit 20
```

See [`docs/daemon.md`](docs/daemon.md) for the full API surface
(socket layout, single-instance semantics, headers, SSE, security
model) and [`docs/plans/20260518-Daemon-UDS-Plan.md`](docs/plans/20260518-Daemon-UDS-Plan.md)
for the design notes. The contract for the `pi --mode rpc` wire
format is summarised in
[`docs/notes/pi-rpc-contract.md`](docs/notes/pi-rpc-contract.md).

## Interactive TUI

`autosk lazy` is a lazygit-style terminal dashboard: tasks, jobs,
workflows, and agents in one process, with a fullscreen inspector
for running pi sessions (`Live / Archive / Meta / Signals` tabs).
It works against `.autosk/db` directly when the daemon isn't
running; the Live tab is the one piece that needs `autosk daemon
serve`. See [`docs/lazy.md`](docs/lazy.md) for the layout diagram,
keymap, filter language, and graceful-degradation contract.

```bash
autosk lazy                    # dashboard
autosk lazy --job run-9ab1     # deep-link straight into the inspector
```

The previous standalone `autosk attach` command is gone in this
release — use `autosk lazy --job <id>` to open a job's live mirror
from the command line.

## Roadmap (post v0.2)

- doltserver backend for multi-writer collaboration
- worktree isolation per run (re-introduces `tasks.git_branch`)
- labels / tags
- full-text search
- audit log table / `autosk history` real implementation
- hooks / plugin events
- import/export / integrations
- comment `--since-step` filtering / token-budget trimming in prompt render
- `autosk lazy`: mouse support, customisable keymaps, theming, multi-project view

## License

MIT — see [LICENSE](LICENSE).
