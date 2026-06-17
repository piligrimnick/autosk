# `autoskd` — the daemon

`autoskd` is the long-running process that drives tasks through their workflows.
It is the **sole owner** of a project's `.autosk/` directory and the single
authority on task state. Every front end — the `autosk` CLI, the `autosk lazy`
TUI, and the Tauri desktop GUI — is a thin **JSON-RPC client** of `autoskd`;
none of them open or write `.autosk/` directly.

`autoskd` is a **Bun/TypeScript** program (it lives in [`daemon/`](../daemon)).
For distribution it is compiled to a standalone binary with `bun build
--compile`, which embeds the Bun runtime — **no global `bun` is needed at
runtime**.

> **Clean break from v1.** `autoskd` stores tasks as **files** under `.autosk/`.
> It does **not** read the old `.autosk/db` database; there is no migrator. A
> project that still has a v1 `.autosk/db` must be opened with the last v1
> release (`v0.1.6`). See [the README clean-break note](../README.md#a-clean-break-from-v1).

## What lives in `.autosk/`

A project is just a directory that contains an `.autosk/` folder, resolved by
walking up from your current directory. There is **no database** — everything is
plain files the daemon writes atomically (tmp + rename):

```
.autosk/
  tasks/<id>/task.json        # one task: title, description, status, workflow, step, blocked_by, timestamps
  tasks/<id>/comments.jsonl   # the task's comments (one JSON object per line)
  sessions/<session-id>.json  # session meta (one agent run for one step)
  sessions/<session-id>.jsonl # the session transcript (pi-format; see "Sessions" below)
  extensions/                 # (optional) project-local extensions: workflows + agents as code
  settings.json               # (optional) extension entries to load (npm:<spec> or a local path)
```

Because tasks are files, you can read or hand-edit them. The daemon picks up
external edits via a startup scan + filesystem watcher (see [Hybrid file
ownership](#hybrid-file-ownership) below). The registry of known projects lives
at `~/.autosk/projects.json`.

## Running it

### Auto-spawn (the normal path)

You almost never start `autoskd` by hand. The first time a front end needs the
daemon, it **auto-spawns** one and waits for it to come up:

1. resolve the `autoskd` binary: `$AUTOSKD_BIN` → a sibling of the calling
   `autosk` binary → `autoskd` on `PATH`;
2. spawn `autoskd serve --sock <sock>` detached;
3. connect over the Unix socket once it is listening.

`autoskd`'s single-instance binding makes a double-spawn harmless: a second
launcher that loses the bind race exits `0` and the client uses the daemon that
won. The Homebrew formula installs `autoskd` right next to `autosk`, so
auto-spawn works with zero configuration.

### Foreground / explicit

To run a daemon yourself (e.g. as a service, or to watch its logs), run the
binary directly:

```bash
autoskd                      # serve on the default socket
autoskd serve --sock /tmp/autosk.sock
autoskd serve --tcp 0.0.0.0:7777     # also listen on TCP (token auth, see below)
autoskd serve --workers 8            # worker-pool size (default 4)
```

`serve` is the only (and default) verb. Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--sock PATH` | `$AUTOSK_SOCK` → `~/.autosk/daemon.sock` | Unix-domain socket path. |
| `--tcp [HOST:]PORT` | off | Also accept connections over TCP (token auth). Bare `PORT` binds `127.0.0.1`. |
| `--workers N` | `4` | Size of the global FIFO worker pool (shared across all projects). |

Environment knobs:

| Variable | Meaning |
| --- | --- |
| `AUTOSK_SOCK` | UDS path (when `--sock` is not passed). |
| `AUTOSK_IDLE_SECS` | Idle-shutdown window in seconds (default `1800`; `0` or negative disables; ignored in TCP mode). |
| `AUTOSK_TOKEN_FILE` | Path to the TCP auth token file (default `~/.autosk/daemon-token`). |
| `AUTOSK_NPM_BIN` | `npm` binary used for every extension install — the first-run bootstrap, the auto-install reconcile, the registry version check + re-install behind `autosk ext update`, and an explicit `autosk ext add npm:<spec>` (default `npm` on `PATH`). |
| `AUTOSK_NO_AUTO_INSTALL` | When set (to any value other than empty / `0` / `false`), disables the automatic first-run bootstrap *and* the reconcile pass; explicit `autosk ext add` / `autosk ext update` still work. |
| `AUTOSKD_BIN` | (front-end side) explicit path to the `autoskd` binary for auto-spawn. |

### One daemon per host, many projects

A single `autoskd` serves **any number of projects**. Each request carries a
`{cwd}` selector; the daemon walks up from that cwd to find the project's
`.autosk/`, opens it lazily (file store + extension registry + scheduler), and
keeps it loaded. The worker pool is global and FIFO across every loaded project.

## What the daemon does for each task

The engine has exactly one scheduling rule:

> a task in `status=work` whose current step is an **agent step** (not a
> `statusStep`) and has no live session ⇒ start a **session** that runs that
> agent's `onRun`.

For each such task:

1. If the workflow declares an **isolation** provider, `acquire` an environment
   (e.g. a git worktree) and run the session inside its `cwd`.
2. Create a session (`sessions/<id>.json` + `.jsonl`) and run the agent's
   `onRun` on the worker pool. The agent writes pi-format transcript entries as
   it works.
3. The agent must call `ctx.transit(target)` exactly once — a sibling step, or a
   terminal/park status (`done` / `cancel` / `human`). `transit` validates the
   target through the workflow's `onTransit` hook, atomically updates `task.json`,
   fires the status-driven isolation lifecycle (nothing on step→step; `release`
   when the task leaves `work`; `reap` on a `done`/`cancel` terminal — see
   [workflows.md → Isolation](workflows.md#isolation-pluggable-per-workflow)),
   and emits notifications.
4. If `onRun` returns **without** transiting, the session fails
   (`error="agent_did_not_transit"`) and the task is parked to `human`.

The loop is **event-driven** (it re-scans on every transit) with a slow safety
rescan as a backstop — there is no 2-second polling.

**Crash recovery.** On startup, any session left `running`/`queued` by a previous
daemon is sealed `failed: daemon_restart` and its task is parked to `human` —
interrupted work is never silently resumed.

## Sessions & transcripts

One invocation of an agent's `onRun` for one task step = one **session**.
Sessions replace v1's "jobs".

- **Meta** (`sessions/<id>.json`): `{ id, kind: task|interactive, task_id,
  workflow, step, agent, status: queued|running|done|failed|aborted, error?,
  started_at, ended_at }`. A `task` session is created by the scheduler for a
  workflow step; an `interactive` session is a taskless chat (see [Interactive
  sessions](#interactive-taskless-sessions) below) whose
  `task_id`/`workflow`/`step` are the empty-string sentinel (`""`).
- **Transcript** (`sessions/<id>.jsonl`): a line 1 header followed by typed
  entries, in a format that **deliberately mirrors pi's session format** so
  pi-based agents can pipe pi entries through verbatim and pi renderers stay
  reusable:
  - **`message` entries** — pi's message schema (`role`, `content[]` blocks incl.
    `text` / `thinking` / `toolCall` / `image`, `usage`/`cost`, `stopReason`).
  - **`custom` entries** — the generic agent logging channel.
  - **engine structural entries** — autosk-specific custom types the engine
    emits itself: `autosk:transit`, `autosk:steer`, `autosk:error`,
    `autosk:session_end` — so a transcript is self-contained.

There is **no retention/GC** in this version: session files accumulate, and
cleanup is manual (`rm .autosk/sessions/…`).

You can steer or abort a **live** session:

- `session.input {kind: "steer"|"followup"}` injects a message into the running
  agent (if the agent supports it);
- `session.abort` fires the session's `AbortSignal`, seals the meta `aborted`,
  and parks the task to `human`.

## Interactive (taskless) sessions

Not every session belongs to a task. An **interactive session** is a taskless
chat you open directly against a registered agent (the GUI's Sessions panel `＋`
button) and drive turn-by-turn — there is no workflow, no synthetic task, and no
`ctx.transit`. It reuses the same `Session` entity, transcript format, and
steer/subscribe surface as a task session; only `kind: "interactive"` and the
`""` sentinels for `task_id`/`workflow`/`step` distinguish it.

**Agent registry.** An extension publishes a named agent with
`AutoskAPI.registerAgent({ name, description?, agent })` (see
[docs/workflows.md → Named agents](workflows.md#named-agents--interactive-sessions)).
`registry.agent.list` returns every registered agent; the GUI picker lists them.
The shipped `@autosk/pi-agent` registers a `"pi"` agent (chat backed by
`pi --mode rpc`).

**Lifecycle.**

1. `session.create {agent}` resolves the named agent (unknown name → invalid
   params), creates a `kind:interactive` session with `cwd = projectRoot` (no
   isolation), and dispatches it **directly** — interactive sessions run **off**
   the bounded task-worker pool, so an idle chat never occupies a slot a task
   session needs, and the scheduler is never involved.
2. The session opens **empty** (no first prompt); the first composer message
   starts the first turn. `session.input {kind:"followup"}` delivers each turn —
   idle → a fresh turn, streaming → a mid-turn follow-up.
3. `session.end` winds the agent down gracefully and seals the session `done`
   (distinct from `session.abort`, which seals `aborted`). Neither parks a task —
   there is none. An interrupted interactive session is sealed
   `failed: daemon_restart` on the next daemon start (again, no park); v1 does
   **not** auto-resume it.

A live interactive session counts as pending work, so an idle (waiting-for-user)
chat keeps the daemon from idle-shutting-down until the chat is ended or aborted.
(Interactive sessions run off the worker pool, so they are **not** reflected in
`meta.healthz`'s `running` counter, which reports task-pool jobs only.)

## Hybrid file ownership

The daemon is the writer for all RPC-driven mutations, but it also honours
external (human/script) edits picked up by its startup scan + fs watcher:

- external edits to `title` / `description` / `blocked_by` / comments are
  accepted as-is;
- external edits to `status` / `step` / `workflow` of a task **with a live
  session** are rejected (the file is rewritten from engine state and a warning
  is logged) — the engine owns enrolled tasks.

## JSON-RPC v2 surface

The protocol is JSON-lines over the transport: one JSON object per line. Every
project-scoped method carries a `{cwd}` selector; all timestamps on the wire are
RFC3339 UTC. The wire types are defined once in
[`daemon/sdk/src/proto.ts`](../daemon/sdk/src/proto.ts) and mirrored by the Go
(`internal/daemon/api`) and Tauri (`gui/src-tauri`) clients.

| Domain | Methods |
| --- | --- |
| meta | `version`, `auth`, `healthz`, `shutdown` |
| project | `list`, `add`, `remove`, `init`, `diagnostics` (extension load errors), `subscribe`/`unsubscribe` |
| task | `list`, `get`, `create`, `update`, `enroll {workflow}`, `resume {to?}`, `done`, `cancel`, `reopen`, `block`/`unblock`, `comment.add/list/edit/delete`, `subscribe`/`unsubscribe` |
| registry | `workflow.list`, `workflow.get`, `agent.list` (rendered from code — read-only) |
| session | `list {task_id?}`, `get`, `transcript {from_line?, limit?}`, `subscribe`/`unsubscribe` (replay-then-tail), `input {message, kind}`, `abort`, `create {agent}` (open an interactive session), `end` (gracefully end one → `done`) |

Notifications (server→client push): `task-changed`, `project-changed`,
`session-event` (`message`|`status`|`done`|`error`). These are fed by engine
events and the fs watcher (so external file edits surface too).

**Error codes.** The reserved JSON-RPC range carries protocol failures; the
domain errors live in the `1xxx` range: `PROJECT_NOT_FOUND` (1001),
`INVALID_PROJECT` (1002), `NOT_FOUND` (1003), `CONFLICT` (1004 — the entity
exists but isn't in a state that accepts the request now; retryable).

A few RPC semantics worth knowing:

- `task.done` / `task.cancel` / `task.reopen` are **administrative overrides**:
  they write via the store and do **not** run `workflow.onTransit` (they reject a
  task with a live session with `CONFLICT`).
- `project.remove` is **lazy**: it forgets the project in the registry and emits
  `project-changed`, but leaves an already-open handle running until the next
  daemon start.

## Transports, auth, single-instance, idle-shutdown

- **UDS (default).** A Unix-domain socket; parent dir `0700`, socket `0600`. No
  auth — filesystem permissions are the gate. Single-instance is enforced by an
  atomic pidfile lock (`<sock>.lock`, with dead-pid reclaim).
- **TCP (opt-in).** `--tcp [HOST:]PORT` adds a TCP listener gated by a token: the
  first request on a TCP connection must be `meta.auth {token}`. The token lives
  at `~/.autosk/daemon-token` (`$AUTOSK_TOKEN_FILE`) and is created on first use.
  Remote front ends (e.g. the GUI in remote mode) use this; a remote daemon must
  be started explicitly (you can't auto-spawn across hosts).
- **Idle-shutdown.** A UDS-mode daemon shuts itself down after the idle window
  (`AUTOSK_IDLE_SECS`, default 30 min) when there are no live connections, no
  queued/running sessions, and no `status=work` tasks across loaded projects.
  Disabled with `0`, and always off in TCP mode (a remote daemon is a service).

## Inspecting the daemon from the CLI

```bash
autosk session list [--task ID]      # sessions in this project (one row per agent run)
autosk session get <id>              # one session's meta
autosk session transcript <id>       # render the pi-format transcript
autosk session abort <id>            # abort a live session (parks the task to human)
autosk session input <id> "<msg>"    # steer (or --followup) a live session

autosk project list                  # known projects on this host
autosk project diagnostics           # extension load errors for this project

autosk version                       # CLI + daemon version
```

The Tauri GUI and `autosk lazy` render the same surface live via the subscribe
streams.
