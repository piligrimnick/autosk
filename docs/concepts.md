# Core concepts & data model

This page is the conceptual + reference foundation for autosk: what a task *is*,
the status machine it moves through, how blockers and the *ready set* work, the
free-form `metadata` bag, comments as the cross-agent channel, and the on-disk
`.autosk/` layout you can read and hand-edit. Read it once and the CLI, the
`autosk lazy` TUI, the desktop GUI, and the workflow engine all make sense.

Where this page describes a surface in depth elsewhere, it links there:
[docs/daemon.md](daemon.md) (the `autoskd` process + RPC), and
[docs/workflows.md](workflows.md) / [docs/extensions.md](extensions.md)
(workflows and agents as code).

---

## What autosk is

autosk is three things wearing one coat:

1. **A task tracker** — small, local, file-based. Tasks live as files under
   `.autosk/` in your repo. Use it as a backlog and to scope an agent's
   attention to concrete context.
2. **A workflow engine** — each workflow is a directed graph of **steps**, each
   step owned by an **agent** the daemon runs. Workflows and agents are **code**
   registered by [extensions](extensions.md), not database rows.
3. **An interface** — the `autosk` CLI, the `autosk lazy` TUI, and the
   desktop/mobile GUI: three ways to manage and observe the same state.

You can stop at step 1 if all you want is a backlog. Step 2 is opt-in: a task
sits in `new` until you enroll it into a workflow.

### One daemon, three pure clients, no database

There is exactly one moving part that owns state: **`autoskd`**, a long-running
Bun/TypeScript process compiled to a standalone binary. It is the **sole owner**
of a project's `.autosk/` directory and the single authority on task state. The
three front ends — the Go CLI, the Go TUI, and the Tauri GUI — are **pure
JSON-RPC clients**; none of them open or write `.autosk/` directly. The daemon is
**auto-spawned on first use**, and **one daemon per host serves any number of
projects** (each request carries a `{cwd}` selector). See
[docs/daemon.md](daemon.md).

There is **no database** — everything is plain files the daemon writes
atomically. There is no Rust workspace and no embedded storage engine anywhere.

```
  autosk (CLI)  ─┐
  autosk lazy   ─┤── JSON-RPC ──▶  autoskd  ──▶  <project>/.autosk/  (files)
  desktop GUI   ─┘                (one per host)
```

> **Clean break from v1.** autosk v2 stores tasks as **files** under `.autosk/`.
> It does **not** read the old `.autosk/db` database, and there is **no
> migrator**. Open an existing v1 project with the last v1 release
> ([`v0.1.6`](https://github.com/wierdbytes/autosk/releases/tag/v0.1.6)); v2
> treats a directory as a fresh project.

---

## The task model

A task is the atom of autosk. It has a stable **id**, a **title**, a free-text
**description**, a **status**, an optional **workflow/step** position, a list of
**blockers**, a free-form **metadata** bag, and two **timestamps**.

### Identity

A task id is **`ask-` followed by 6 lowercase hex characters** — e.g.
`ask-3f9b2c` (`daemon/sdk/src/ids.ts`). It is generated at create time and never
changes. The **task's directory name is the canonical id**: even if you
hand-edit the `id` field inside `task.json`, the daemon ignores it and uses the
directory name (the file's `id` self-heals on the next write).

### Fields

The on-disk record (`tasks/<id>/task.json`) carries exactly these fields:

| Field | Type | Notes |
| --- | --- | --- |
| `id` | string | `ask-XXXXXX`; equals the task directory name. |
| `title` | string | Short summary. |
| `description` | string | Free text (`""` when empty). |
| `status` | enum | One of `new` / `work` / `human` / `done` / `cancel` — see [the status machine](#the-task-status-machine). |
| `workflow` | string \| null | The enrolled workflow name; `null` until enrolled. |
| `step` | string \| null | The current step within that workflow; `null` until enrolled. |
| `blocked_by` | string[] | Ids of tasks this one waits on — see [blockers](#blockers--the-dependency-graph). |
| `metadata` | object | Free-form bag; **omitted from disk when empty** — see [metadata](#task-metadata--step_visits). |
| `created_at` | string | RFC3339 UTC. |
| `updated_at` | string | RFC3339 UTC; bumped on every mutation. |

### The rendered view vs. the stored record

Clients never read `task.json` directly — they read a **`TaskView`** the daemon
derives server-side (`daemon/sdk/src/types.ts`). The view is the stored record
plus three **derived** fields that are *never stored*:

- **`blocked`** — `true` when at least one blocker is still open.
- **`blocked_by`** — the blockers as `{ id, status }` references (not bare ids).
- **`blocks`** — the tasks *this one* blocks (the reverse edge).
- **`comment_count`** — the number of comments on the task.

So `blocked_by` is a flat `string[]` on disk but a richer `{ id, status }[]` on
the RPC wire. (The `autosk --json` CLI output flattens the edges back to id
strings for compactness; the proto-v2 `TaskView` keeps the `{ id, status }`
shape — see [docs/daemon.md → JSON-RPC v2 surface](daemon.md#json-rpc-v2-surface).)

### Timestamps

`created_at` / `updated_at` are **RFC3339 UTC** on the wire and on disk. The Go
front ends render them in your **local timezone** for human output (via
`internal/timeformat`), but the machine surfaces — the `--json` CLI output, the
proto-v2 wire types, and the on-disk files — stay UTC. Don't be surprised when
`autosk show` prints a different clock time than `autosk show --json`.

### Example

A freshly-created, unenrolled task (`metadata` omitted because it is empty):

```json
{
  "id": "ask-7a1e4d",
  "title": "Tidy the README",
  "description": "",
  "status": "new",
  "workflow": null,
  "step": null,
  "blocked_by": [],
  "created_at": "2026-06-24T09:00:00Z",
  "updated_at": "2026-06-24T09:00:00Z"
}
```

The same task once enrolled and picked up by the engine (note the non-empty
`metadata.step_visits`):

```json
{
  "id": "ask-3f9b2c",
  "title": "Wire up the auth flow",
  "description": "",
  "status": "work",
  "workflow": "feature-dev",
  "step": "dev",
  "blocked_by": [],
  "metadata": {
    "step_visits": {
      "dev": 1
    }
  },
  "created_at": "2026-06-24T09:12:03Z",
  "updated_at": "2026-06-24T09:14:55Z"
}
```

The rendered view, human form (local timezone):

```text
$ autosk show ask-3f9b2c
[ask-3f9b2c]: Wire up the auth flow
status:        work
workflow:      feature-dev
step:          dev
blocked:       no
blocked_by:    -
blocks:        -
comments:      0
created_at:    2026-06-24 12:14:55
updated_at:    2026-06-24 12:14:55
description:   -
```

…and the machine form (RFC3339 UTC, edges as id strings):

```json
$ autosk show ask-3f9b2c --json
{"id":"ask-3f9b2c","title":"Wire up the auth flow","description":"","status":"work","workflow":"feature-dev","step":"dev","created_at":"2026-06-24T09:14:55Z","updated_at":"2026-06-24T09:14:55Z","blocked":false,"blocked_by":[],"blocks":[],"comment_count":0,"metadata":{"step_visits":{"dev":1}}}
```

---

## The task status machine

Every task is in exactly one of five statuses:

| Status | Meaning |
| --- | --- |
| `new` | Open work, not enrolled in a workflow. |
| `work` | Enrolled and owned by the engine — an agent is (or will be) on it. |
| `human` | Parked, waiting for a person (a `human` step, a park, or a failure). |
| `done` | Completed. |
| `cancel` | Abandoned. |

`new`, `work`, and `human` are the **open** statuses; `done` and `cancel` are
**terminal**. This open/terminal split is what blockers key off (a terminal
blocker no longer blocks — see [below](#blockers--the-dependency-graph)).

### How a task moves between statuses

- **`new` → `work`** — `autosk enroll <id> --workflow <name>` (or
  `autosk create … --workflow <name>`) puts the task into a workflow at its
  `firstStep`. Allowed from `new`, `cancel`, or `human`.
- **`work` → step / `work` → terminal/park** — the **engine** drives these as
  the workflow's agent calls `ctx.transit(...)`. A transition into a
  `statusStep("human")` parks the task; `statusStep("done")` /
  `statusStep("cancel")` close it. See [docs/workflows.md](workflows.md).
- **`human` → `work`** — `autosk resume <id>` re-enters the parked step;
  `autosk resume <id> --to <step|status>` re-enters a chosen target.
- **administrative overrides** — `autosk done <id>` / `autosk cancel <id>` /
  `autosk reopen <id>` flip the status directly. These do **not** run the
  workflow's `onTransit` hook and are rejected (`CONFLICT`) for a task with a
  **live session** (the engine owns enrolled tasks while they run). `reopen`
  returns a never-enrolled task to the `new` backlog, and parks a task that has a
  workflow to `human` (so you can `resume` it).

A task whose agent run fails to transition cleanly is **parked to `human`**, not
left dangling — interrupted work always lands somewhere a person can pick it up.

### The ready set

> **The ready set is the tasks in `new` status with no open blocker.**

That is what humans and agents pull from — "what should I work on right now?".
There is no dedicated "ready" entity or RPC method: the ready set is just a
task list filtered by `status = new` and `blocked = false`
(`cmd/autosk/ready.go`).

```bash
autosk ready              # the whole ready set
autosk ready --limit 5    # cap the rows
autosk next               # the single top ready task (== ready --limit 1)
```

`autosk next` prints `null` (JSON) or `(nothing ready)` and exits non-zero when
the ready set is empty, so it composes in scripts:

```bash
id=$(autosk next --json | jq -r .id) && autosk enroll "$id" --workflow feature-dev
```

Note that an enrolled (`work`) task is **never** in the ready set even if it is
unblocked — once the engine owns a task, it is not something you pull by hand.

---

## Blockers & the dependency graph

A task can declare that it **waits on** other tasks. On disk this is the flat
`blocked_by` string array of task ids; the daemon derives the rest of the graph.

```bash
autosk block   ask-3f9b2c ask-7a1e4d   # ask-3f9b2c now waits on ask-7a1e4d
autosk unblock ask-3f9b2c ask-7a1e4d   # remove that single edge
autosk unblock ask-3f9b2c --all        # remove every blocker of ask-3f9b2c
autosk dep list ask-3f9b2c             # show blocked_by + blocks for the task
```

Both `block` and `unblock` are **idempotent** (adding a duplicate edge or
removing a missing one is a no-op). A task **cannot block itself** — a self-edge
is rejected, because it would leave the task permanently blocked.

### Derived `blocked`, `blocked_by`, and `blocks`

The daemon computes three things from the `blocked_by` edges across the whole
project (`daemon/core/src/store/store.ts`):

- **`blocked`** is `true` when **any** blocker is still in an open status
  (`new` / `work` / `human`). A blocker that reached `done` or `cancel` no longer
  blocks, so finishing a dependency automatically un-gates the tasks that waited
  on it — they re-enter the ready set with no extra action.
- **`blocked_by`** renders each blocker as `{ id, status }` so a client can show
  *why* a task is blocked.
- **`blocks`** is the reverse edge: the tasks that name *this* one in their
  `blocked_by`. It is computed from a reverse index, never stored.

### Dangling edges are hidden, not invented

You can `block` a task against a blocker id that **doesn't exist yet** — the edge
is stored as-is (blockers may be created later). Until the blocker exists, the
daemon **hides** the dangling edge from the rendered `blocked_by` / `blocks` and
does not count it toward `blocked`. The moment a matching task appears, the edge
lights up. This lets you wire up a dependency before its blocker is filed without
a task being mysteriously, permanently blocked by a typo.

In the compact task table, a blocked task shows its status with a trailing `*`
(here `ask-3f9b2c` is blocked, `ask-7a1e4d` is not):

```text
$ autosk list
ID          STATUS  TITLE
ask-3f9b2c  new*    Wire up the auth flow
ask-7a1e4d  new     Tidy the README
```

---

## Task metadata & `step_visits`

Every task carries a free-form **`metadata`** object — an opaque, human-editable
key/value bag. It is always present in memory and on the wire (an empty object
`{}` when the task has none), but **omitted entirely from `task.json` when
empty**, so a task that never carried metadata serialises byte-for-byte like the
pre-metadata format. A missing / corrupt / non-object `metadata` on disk parses
defensively back to `{}`.

```bash
autosk metadata show  <id>                  # pretty JSON (honors --json)
autosk metadata set   <id> owner alice      # value parsed as JSON, else a string
autosk metadata set   <id> sprint 42        # -> the number 42
autosk metadata unset <id> owner            # remove a key
```

`metadata set` takes **dot-paths**, so you can write into nested objects, and the
daemon creates the intermediate objects for you:

```bash
autosk metadata set <id> labels.priority high   # { "labels": { "priority": "high" } }
```

The bag is opaque to the daemon **except for one reserved sub-object**:

### `step_visits` — the reserved engine counter

`metadata.step_visits` is a `{ step name → entry count }` map the **engine
auto-maintains** for workflow visit caps. On **every transition into a named
step** — enroll → `firstStep`, a step→step transit, a `resume` into a step — the
engine bumps `step_visits[step]` by one, atomically with the position write. (A
terminal/park `{ status }` flip, and the administrative `reopen`, do **not**
count.)

A workflow reads this counter via `ctx.visits(step)` to cap how many times a task
can bounce through a step. For example, the reference `feature-dev` workflow
parks a task for a human once it has entered `dev` five times instead of looping
forever. See [docs/workflows.md → `onTransit`](workflows.md#ontransit--the-only-graph-authority).

Because the counter lives in human-editable metadata, **it is resettable** —
this is the supported escape hatch for a task that hit a visit cap but
legitimately needs more passes:

```bash
autosk metadata unset <id> step_visits        # reset every step's count
autosk metadata set   <id> step_visits.dev 0  # reset just the dev count
```

Only the dedicated `metadata set` / `metadata unset` family writes the bag —
there is no `task update` passthrough for metadata.

---

## Comments — the cross-agent channel

Comments are how work is communicated across agents (and humans) on a task. They
are stored as **`tasks/<id>/comments.jsonl`** — one JSON object per line:

```jsonc
{"id":"cm-1a2b3c","author":"dev","text":"Implemented the token refresh path.","created_at":"2026-06-24T09:30:00Z","updated_at":"2026-06-24T09:30:00Z"}
```

A comment id is **`cm-` followed by 6 lowercase hex characters**, collision-
checked against the ids already on that task (the id is the edit/delete key, so a
duplicate within one task would retarget the wrong comment).

> **Comments are the cross-agent channel.** The workflow engine surfaces **every
> prior comment at the top of each step's prompt**. So a `dev` agent's comment is
> context the `review` agent reads before it starts. Leaving a clear comment is
> how an agent hands off to the next step (and how *you* leave instructions for
> an agent).

```bash
autosk comment add  <id> "Refactored the retry loop; see PR notes."
autosk comment add  <id> -            # read the body from stdin
autosk comment list <id>              # oldest first
autosk comment edit   <id> <comment-id> "new text"
autosk comment delete <id> <comment-id>
```

The **author** defaults to **`$AUTOSK_AGENT`** (or `human` when that env var is
unset); override it with `--author`. Each enrolled agent runs with
`AUTOSK_AGENT` set to its step name, which is why comments are attributed to
`dev` / `review` / … automatically.

Unlike v1, comments are **editable and deletable** (not strictly append-only).
The daemon is the sole writer in the normal path — but because the file is plain
JSONL under [hybrid ownership](#manual-editing--hybrid-file-ownership), a
fat-fingered hand-edit only damages that one line: the parser skips a malformed
line rather than failing the whole read, and logs a one-time warning.

---

## On-disk `.autosk/` layout

A project is just a directory that contains an `.autosk/` folder. The daemon
finds it by **walking up** from the request's `{cwd}` to the nearest ancestor
that has an `.autosk/` directory (`daemon/core/src/project/resolve.ts`), so you
can run `autosk` from any subdirectory of your repo. The resolved root is
canonicalised (symlinks resolved) so every client agrees on one key per project.

```
<project-root>/
  .autosk/
    tasks/<id>/task.json        # one task (see "The task model" above)
    tasks/<id>/comments.jsonl   # that task's comments (one JSON object per line)
    sessions/<session-id>.json  # session meta (one agent run for one step)
    sessions/<session-id>.jsonl # the session transcript (pi-format)
    extensions/                 # (optional) project-local workflows + agents as code
    settings.json               # (optional) extension entries to load
```

`autosk init` (or the implicit first-write auto-init) lays down the `tasks/`,
`sessions/`, and `extensions/` skeleton. The registry of **known projects** on a
host lives separately at `~/.autosk/projects.json` (resolution itself never
auto-registers a project — that is `project.add`'s job).

For the session files (`sessions/`), the extension settings (`settings.json`,
`extensions/`), and the daemon-level details, see [docs/daemon.md](daemon.md) and
[docs/extensions.md](extensions.md). There is **no retention/GC** of session
files in this version — cleanup is manual.

### Manual editing & hybrid file ownership

Because tasks are plain files, you (or a script) can read and **hand-edit** them.
The daemon picks up external edits via a startup scan plus a filesystem watcher,
and writes are atomic (write-to-temp + rename), so a reader never sees a
half-written file.

The daemon is the writer for all RPC-driven mutations, but it **honours external
edits** under a clear ownership split:

- **Human-owned fields — accepted as-is:** external edits to `title` /
  `description` / `blocked_by` / `metadata`, and to `comments.jsonl`.
- **Engine-owned fields — protected during a run:** external edits to `status` /
  `step` / `workflow` of a task **with a live session** are **rejected** — the
  daemon rewrites the file from engine state and logs a warning. The engine owns
  an enrolled task only while a session is live; a parked (`human`) task, or the
  gap between sessions, accepts these edits too.

This is why hand-editing (or `autosk metadata unset`) the `step_visits` counter
is a safe escape hatch: `metadata` is a human-owned field, so your edit sticks
(last-writer-wins against a concurrent engine bump). A corrupt `task.json` on an
idle task is dropped from views (the bad bytes are left on disk for you to fix)
rather than bricking the whole project listing.

See [docs/daemon.md → Hybrid file ownership](daemon.md#hybrid-file-ownership) for
the full reconciliation rules.

---

## Where to next

- **Run every verb** → [docs/cli.md](cli.md) (the `autosk` CLI reference:
  global behavior, all verbs + flags, env vars, scripting recipes).
- **Drive tasks through a pipeline** → [docs/workflows.md](workflows.md)
  (workflows & agents as code, the `feature-dev` reference workflow, isolation).
- **Teach a project new workflows/agents** → [docs/extensions.md](extensions.md).
- **The process that owns it all** → [docs/daemon.md](daemon.md) (`autoskd`,
  auto-spawn, the JSON-RPC surface, sessions & transcripts).
- **Manage and observe interactively** → [docs/lazy.md](lazy.md) (the TUI) and
  [gui/README.md](../gui/README.md) (the desktop/mobile GUI).
