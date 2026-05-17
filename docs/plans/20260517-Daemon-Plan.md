# autosk — daemon / pi-orchestrator plan

**Date:** 2026-05-17
**Status:** Spec locked (ask_user round 2026-05-17), ready for scaffold.
**Predecessors:** [`20260513-Init-Plan.md`](20260513-Init-Plan.md),
[`20260513-Impl-Plan.md`](20260513-Impl-Plan.md).

---

## 1. Purpose

Add a **daemon / orchestrator mode** to autosk that, given an autosk task id
(or a raw prompt), spawns `pi` in long-lived RPC mode to work on the task,
exposes job lifecycle and recent messages over an HTTP API, and (optionally)
ties the run back to the autosk task with `claim` / `done`.

The daemon is the smallest viable surface that turns autosk from a "todo
list with a CLI" into "todo list + workers". Multi-writer collaboration,
remote agents, and pluggable runners are explicitly out of scope.

---

## 2. Decisions (locked)

| Topic | Decision |
|---|---|
| Where the code lives | `autosk daemon` subcommand (+ `internal/daemon/`) — single binary. |
| Transport | HTTP + JSON (REST-style). |
| Concurrency | N parallel workers, configurable. |
| Task binding | Accept `as-XXXX` (server pulls title/description and renders the prompt) **or** raw prompt for ad-hoc runs. |
| autosk integration | Auto-`claim` on start. **Never** auto-`done` — closing the task is the subagent's responsibility (via the `autosk` pi extension). The daemon **verifies** closure after every pi turn and **kicks the agent back** with a corrective user message when the task is not closed per protocol. |
| Valid closure (verified on every end-of-turn) | One of: (a) `tasks.status = done`; (b) `tasks.status = cancelled`; (c) the task gained at least one new blocker since the run started (treated as "decomposed into subtasks"). |
| Kickback on invalid closure | Send a corrective user message in the same pi RPC session asking the agent to close per protocol. Up to `max_corrections` attempts (default `3`), then the run terminates as `failed` with `error="agent_did_not_close_task"`. |
| pi session linkage | Persist `pi_session_id` and `session_path` on the run row. No comments/metadata column on `tasks` (no schema change to `tasks`); the run row is the source of truth. |
| Schema | New table `daemon_runs`. Compatibility with old DBs is **not** preserved — migration runs forward; existing v0.1 DBs gain the table on next `autosk migrate`. |
| Status API | `lifecycle + exit_code + duration`, **tail** of last N messages, **full transcript** on request, **SSE** stream of new messages. |
| pi invocation | Long-lived `pi --mode rpc`; messages sent over stdin; one pi process per active job. |
| Cancel | `DELETE /v1/jobs/{id}` → SIGTERM → grace period → SIGKILL. |
| Run state storage | New `daemon_runs` table in the same `.autosk/db`. |
| Bind | Configurable `bind` and optional `token`; **default: `127.0.0.1:7878`, no auth**. |
| Working dir | `cwd` field in the request; **default: the project root** (the directory containing `.autosk/`). |
| Scope of v0 | "MVP+": submit / status / messages / cancel / list + N workers + autosk claim/done auto-integration. |

Anything not on this list is post-v0 (see §10).

---

## 3. High-level shape

```
┌───────────────────────────────────────────────┐
│            autosk daemon serve                │
│  ┌─────────────────┐    ┌─────────────────┐   │
│  │  HTTP API       │───▶│  Scheduler      │   │
│  │  /v1/jobs*      │    │  (queue + N     │   │
│  │  /v1/jobs/{}/…  │◀───│   workers)      │   │
│  └─────────────────┘    └────────┬────────┘   │
│           │                      │            │
│           ▼                      ▼            │
│  ┌─────────────────┐    ┌─────────────────┐   │
│  │ DaemonStore     │    │ pi runner       │   │
│  │ daemon_runs +   │    │ (long-lived RPC │   │
│  │ tasks (via      │    │  child process) │   │
│  │ existing Store) │    └────────┬────────┘   │
│  └─────────────────┘             │            │
└────────────────────────────────┬─┘            │
                                 ▼              │
                       pi   --mode rpc          │
                            --model ...         │
                            --thinking ...      │
                            --session-dir ...   │
                                 │              │
                                 ▼              │
                  ~/.../sessions/<job>.jsonl ───┘ (tailed for transcript)
```

Single Go binary. The HTTP server, scheduler, and pi runners all live in
the same process. State is persisted in `.autosk/db` so a CLI client
(`autosk daemon status <job-id>`) can also read it directly when the
daemon happens to be down.

---

## 4. Data model

### 4.1 New table `daemon_runs`

Added by migration `002_daemon_runs.sql`. No changes to `tasks` or
`task_deps`. No `metadata` column on tasks.

```sql
CREATE TABLE daemon_runs (
  job_id           TEXT PRIMARY KEY,           -- "job-XXXXXX" (hex, like task ids)
  task_id          TEXT,                       -- nullable; references tasks(id) ON DELETE SET NULL
  prompt           TEXT NOT NULL,              -- final prompt passed to pi
  model            TEXT NOT NULL DEFAULT '',   -- "" = pi default
  thinking         TEXT NOT NULL DEFAULT ''    -- "" | off | minimal | low | medium | high | xhigh
                   CHECK (thinking IN ('','off','minimal','low','medium','high','xhigh')),
  cwd              TEXT NOT NULL,              -- absolute path
  status           TEXT NOT NULL
                   CHECK (status IN ('queued','running','done','failed','cancelled')),
  exit_code        INTEGER,                    -- nullable, set on terminal status
  pid              INTEGER,                    -- nullable, pi child pid while running
  pi_session_id    TEXT,                       -- nullable, captured from pi
  session_path     TEXT,                       -- nullable, absolute path to session.jsonl
  error            TEXT,                       -- nullable, terminal failure reason
  auto_claim       INTEGER NOT NULL DEFAULT 1, -- bool: did we auto-claim task_id at start
  max_corrections  INTEGER NOT NULL DEFAULT 3, -- max corrective messages before failing the run
  corrections_used INTEGER NOT NULL DEFAULT 0, -- corrective messages sent so far
  closure_kind     TEXT                        -- nullable; set on success only.
                   CHECK (closure_kind IS NULL OR closure_kind IN ('done','cancelled','decomposed')),
  pre_blocked_by   TEXT NOT NULL DEFAULT '',   -- comma-separated snapshot of task.blocked_by at run start;
                                               -- used to detect new blockers ("decomposed" closure)
  created_at       INTEGER NOT NULL,
  started_at       INTEGER,                    -- nullable
  finished_at      INTEGER,                    -- nullable
  FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL
);

CREATE INDEX daemon_runs_task_id  ON daemon_runs(task_id);
CREATE INDEX daemon_runs_status   ON daemon_runs(status, created_at);
CREATE INDEX daemon_runs_created  ON daemon_runs(created_at);
```

**No message duplication.** Recent messages and the full transcript are
served by reading pi's own `session.jsonl` at `session_path`. We store the
path; we do not copy the body. This keeps the DB small and avoids
"who's the source of truth?" drift.

### 4.2 IDs

`job-XXXXXX` (6 hex chars, same generator family as `as-XXXX`), reusing
`internal/id`. Collisions retried on insert; capacity at 6 hex is more than
enough for a local daemon's lifetime.

### 4.3 Restart recovery

At daemon startup, every `daemon_runs` row still marked `running` is
**rewritten** to `failed` with `error="daemon_restart"` and
`finished_at=now`. We do **not** try to re-attach to surviving pi processes.
This is the simplest correct behaviour for v0.

---

## 5. HTTP API

Bind defaults to `127.0.0.1:7878`. JSON in, JSON out, `Content-Type:
application/json`. If `token` is configured, all routes require
`Authorization: Bearer <token>` (or 401).

### 5.1 Routes

| Method | Path | Purpose |
|---|---|---|
| `POST`   | `/v1/jobs`                       | Submit a new job. |
| `GET`    | `/v1/jobs`                       | List jobs, filterable. |
| `GET`    | `/v1/jobs/{job_id}`              | Read a single job (status + metadata). |
| `DELETE` | `/v1/jobs/{job_id}`              | Cancel (SIGTERM → grace → SIGKILL). |
| `GET`    | `/v1/jobs/{job_id}/messages`     | Tail of recent session events. |
| `GET`    | `/v1/jobs/{job_id}/stream`       | Server-Sent Events stream of new events + status changes. |
| `GET`    | `/v1/healthz`                    | `{"ok":true,"workers":2,"queued":0,"running":1}`. |
| `GET`    | `/v1/version`                    | autosk + daemon build info. |

### 5.2 `POST /v1/jobs` — request body

```jsonc
{
  "task_id":   "as-a1b2",        // optional — if present, server builds prompt
  "prompt":    "fix X in Y",     // optional — required when task_id absent;
                                 // when both present, prompt overrides the rendered task prompt
  "model":     "sonnet:high",    // optional; passed to pi as --model
  "thinking":  "high",           // optional; off|minimal|low|medium|high|xhigh
                                 // (also accepts "" for pi default; rejected otherwise)
  "cwd":       "/abs/path",      // optional; default: daemon's project root
  "auto_claim":      true,       // optional; default true when task_id set, ignored otherwise.
                                 // If false, the daemon will not call `autosk claim` at start — the agent owns claim too.
  "max_corrections": 3,          // optional; cap on corrective kickback messages. Ignored when task_id absent.
  "extra_args": ["--no-tools"],  // optional escape hatch, appended to argv after defaults
  "env":        { "FOO": "bar" } // optional, merged onto inherited env (whitelist enforced)
}
```

Validation (server returns `400` with `{ error, details }` on fail):

- At least one of `task_id`, `prompt` must be present.
- `task_id` if present must exist in `tasks`.
- `cwd` if present must be absolute and exist.
- `thinking` must be in the allowed enum.
- `extra_args` must not contain a known dangerous flag (`--api-key`,
  `--system-prompt`, `--mode`, `--session-dir`, `--session`, `--no-session`,
  `--print`, `-p`, `--model`, `--thinking`) — these are managed by the daemon.

### 5.3 Job representation (response shape)

```jsonc
{
  "job_id":        "job-1f2e3d",
  "task_id":       "as-a1b2",          // nullable
  "status":        "running",          // queued|running|done|failed|cancelled
  "model":         "sonnet:high",
  "thinking":      "high",
  "cwd":           "/abs/path",
  "prompt_preview":"fix X in Y\n…",    // first 200 chars
  "pi_session_id": "8c2f…",            // nullable
  "session_path":  "/abs/.autosk/sessions/job-1f2e3d/session.jsonl",
  "pid":           48211,              // nullable
  "exit_code":     null,
  "error":         null,
  "closure_kind":  null,               // nullable until terminal; one of done | cancelled | decomposed on success
  "corrections_used":  0,              // how many kickback messages we sent in this run
  "max_corrections":   3,
  "pre_blocked_by":    ["as-cd3e"],    // blockers present at run start, for "decomposed" detection
  "created_at":    "2026-05-17T15:00:00Z",
  "started_at":    "2026-05-17T15:00:01Z",
  "finished_at":   null,
  "duration_ms":   12345               // computed
}
```

### 5.4 `GET /v1/jobs/{job_id}/messages` — tail

Query params: `?limit=20` (default 20, max 500), `?full=true` for the whole
transcript (no limit). Returns events parsed from `session.jsonl` projected
to a stable shape:

```jsonc
{
  "job_id": "job-1f2e3d",
  "events": [
    { "ts": "...", "kind": "assistant_text", "text": "..." },
    { "ts": "...", "kind": "tool_call", "name": "read", "input": {...} },
    { "ts": "...", "kind": "tool_result", "name": "read", "ok": true, "summary": "..." },
    { "ts": "...", "kind": "status", "value": "running" }
  ],
  "truncated": false
}
```

`kind` values are normalised by the projection layer (`internal/daemon/pi/events.go`)
so the API is decoupled from pi's exact wire schema. We project the wire
schema we observe; new pi event kinds fall back to `kind:"other"` with the
raw object preserved.

### 5.5 `GET /v1/jobs/{job_id}/stream` — SSE

Server-Sent Events. Each event is one of `event: message|status|done`.
Initial fan-out replays the last `limit` events, then streams new ones.
Disconnect-safe (clients may reconnect with `Last-Event-ID`).

### 5.6 List filters

`GET /v1/jobs?status=running&task_id=as-a1b2&limit=50&since=…&order=created_at:desc`.

---

## 6. Job lifecycle

```
   ┌────────┐ worker free   ┌─────────┐  end-of-turn + valid closure   ┌──────┐
   │ queued │ ─────────────▶│ running │ ──────────────────────────────▶│ done │
   └────┬───┘               └──┬──┬───┘                                └──────┘
        │                      │  │ end-of-turn + invalid closure
        │                      │  │  (corrections_used < max)
        │                      │  └────────────┐
        │                      │  send corrective user message
        │                      │  corrections_used += 1
        │                      │◀─────────────┘   (stay in running)
        │                      │
        │                      │ pi exit != 0  / parse error /
        │                      │ idle_timeout  / corrections exhausted   ┌────────┐
        │                      ├────────────────────────────────────────▶│ failed │
        │                      │                                         └────────┘
        │ DELETE               │ DELETE                                  ┌───────────┐
        └──────────────────────┴────────────────────────────────────────▶│ cancelled │
                                                                         └───────────┘
```

Terminal statuses (`done`, `failed`, `cancelled`) are sticky. Any status
can be queried after the daemon is restarted (rows survive in `daemon_runs`).

### 6.1 Worker loop (pseudocode)

```go
for job := range queue {
    run := store.MarkRunning(job.ID)

    // Snapshot blockers BEFORE the agent gets a chance to add new ones.
    if run.TaskID != "" {
        pre, _ := taskStore.Deps(ctx, run.TaskID) // incoming blockers
        store.SetPreBlockedBy(run.ID, pre)
        if run.AutoClaim {
            _ = taskStore.Claim(ctx, run.TaskID) // idempotent, errors logged not fatal
        }
    }

    p := pi.Spawn(ctx, run.Cwd, pi.Opts{
        Model: run.Model, Thinking: run.Thinking,
        SessionDir: sessionDirFor(run.ID), Extra: run.ExtraArgs,
    })
    store.SetPID(run.ID, p.PID)

    err := p.SendUserMessage(run.Prompt)

    // Verify-and-kick-back loop. Every end-of-turn is a checkpoint.
    var closure ClosureKind // "", done, cancelled, decomposed
turnLoop:
    for err == nil {
        if err = p.WaitForTurnEnd(ctx); err != nil { break }

        if run.TaskID == "" {
            // Ad-hoc prompt: no closure to verify, one turn is enough.
            closure = "done"
            break
        }
        closure, verr := verifyClosure(ctx, taskStore, run.TaskID, run.PreBlockedBy)
        if verr != nil { err = verr; break }
        if closure != "" {
            break turnLoop // valid closure observed
        }
        if run.CorrectionsUsed >= run.MaxCorrections {
            err = errAgentDidNotCloseTask
            break
        }
        run.CorrectionsUsed++
        store.IncCorrections(run.ID)
        err = p.SendUserMessage(correctiveMessage(run.TaskID, run.PreBlockedBy))
    }

    p.RequestExit(ctx)                          // ask pi to shut down cleanly
    exitCode, waitErr := p.Wait(ctx, gracePeriod)

    switch {
    case ctxCancelledByDELETE(ctx):
        store.MarkCancelled(run.ID, exitCode)
    case errors.Is(err, errAgentDidNotCloseTask):
        store.MarkFailed(run.ID, exitCode, "agent_did_not_close_task")
    case err != nil || waitErr != nil || exitCode != 0:
        store.MarkFailed(run.ID, exitCode, joinErr(err, waitErr))
    default:
        store.MarkDone(run.ID, exitCode, closure) // closure_kind persisted
    }
}
```

### 6.1.1 `verifyClosure`

```go
// verifyClosure inspects the task right after an end-of-turn and returns:
//   "done"       — tasks.status == done
//   "cancelled"  — tasks.status == cancelled
//   "decomposed" — status still new|claimed BUT incoming blockers grew
//                  relative to the run-start snapshot
//   ""           — agent did not close per protocol; caller will kick back
func verifyClosure(ctx, store, taskID, preBlockedBy) (ClosureKind, error) {
    t, err := store.GetTask(ctx, taskID); if err != nil { return "", err }
    switch t.Status {
    case StatusDone:      return "done", nil
    case StatusCancelled: return "cancelled", nil
    }
    inc, _, err := store.Deps(ctx, taskID); if err != nil { return "", err }
    if hasNew(inc, preBlockedBy) {
        return "decomposed", nil
    }
    return "", nil
}
```

### 6.1.2 Corrective message template

Fixed text, parametrised by task id and the snapshot of pre-existing
blockers, e.g.:

> Your task `as-XXXX` is still open (`status=claimed`) and you have not
> added any new blockers. Before you stop, you must close it per
> protocol: call `autosk done <id>` if the work is complete, or
> `autosk cancel <id>` if it cannot be done, or decompose it via
> `autosk create … --blocks <id>` and stop. This is correction
> attempt N of M.

The exact wording is finalised in D8 and reviewed against the autosk
extension's tool description so the verbs match what the agent has
available.

### 6.2 "Turn end" detection

This is the one place where v0 must observe pi's actual RPC contract. The
known shape (from `pi --mode rpc -p`) is JSON-lines on stdout. The runner
treats one of the following as end-of-turn:

1. A message with `type:"response"` and `success:true` (and not a streaming
   chunk).
2. An idle marker (TBD — to be confirmed in P0 reconnaissance).
3. pi's stdout closes (process exit).

If none of those arrive within `idle_timeout` (default 30 min), the worker
declares the run `failed` with `error="idle_timeout"`. The timeout is
configurable per request via a future field; v0 ships a single global value.

### 6.3 Cancel

On `DELETE`: mark intent in memory, `SIGTERM` the child, wait `grace_period`
(default `10s`), then `SIGKILL`. The cancel handler is idempotent. The run
row transitions to `cancelled` regardless of pi's exit code.

---

## 7. Configuration

CLI flags on `autosk daemon serve` (v0 ships flags only; TOML support is
deferred):

| Flag | Default | Effect |
|---|---|---|
| `--bind` | `127.0.0.1:7878` | listen address |
| `--token-file` | empty | if set, content used as Bearer token |
| `--workers` | `2` | max concurrent pi processes |
| `--cwd` | (project root) | default cwd when request omits it |
| `--grace` | `10s` | cancel SIGTERM→SIGKILL grace |
| `--idle-timeout` | `30m` | max time without an event before failing a run |
| `--pi-bin` | `pi` (looked up on PATH) | override pi binary |
| `--session-dir` | `.autosk/sessions` | parent dir for per-job session subdirs |
| `--log-level` | `info` | `debug|info|warn|error` |

### 7.1 Default-model / default-thinking

Daemon does **not** force a default model. If the request omits `model`,
pi's own default applies. The daemon never injects `--model` / `--thinking`
unless the request sets them.

---

## 8. CLI surface

### 8.1 New commands

```
autosk daemon                            # alias for `daemon serve`
autosk daemon serve   [flags from §7]
autosk daemon submit  <as-id>  [--prompt P] [--model M] [--thinking L] [--cwd D]
autosk daemon submit  --prompt P [--model M] [--thinking L] [--cwd D]
autosk daemon status  <job-id>           [--json]
autosk daemon messages <job-id> [--limit N] [--full] [--json]
autosk daemon cancel  <job-id>
autosk daemon list    [--status s,s | all] [--task-id ID] [--limit N] [--json]
```

`autosk daemon submit|status|messages|cancel|list` are **thin clients** over
the HTTP API. They read `--daemon-url` (default `http://127.0.0.1:7878`)
and `--daemon-token-file` (default empty). They never touch
`daemon_runs` directly. This keeps a single code path and makes the API the
contract.

### 8.2 Wiring into `cmd/autosk/main.go`

One new `newDaemonCmd()` constructor with `serve|submit|status|messages|
cancel|list` subcommands. `daemon serve` lives behind a CGO-free build tag
where possible (the runner doesn't link doltlite directly — it goes through
the existing `store.Store`).

---

## 9. Implementation phases

Each phase ends with a runnable artifact and a tiny self-test. Phases are
sized so a single small autosk task tracks each.

| ID | Phase | Done when |
|---|---|---|
| **D0** | **Reconnaissance: pi RPC contract** | `docs/notes/pi-rpc-contract.md` lists the exact event types we will observe (assistant text, tool_call, tool_result, end-of-turn marker, error). One probe script under `scripts/` reproduces every kind. |
| **D1** | **Schema + migration** | `002_daemon_runs.sql` ships; `autosk migrate` applies cleanly on a fresh and on a v0.1 DB; conformance tests assert table presence. |
| **D2** | **DaemonStore layer** | `internal/daemon/store/*` exposes `CreateRun`, `GetRun`, `ListRuns`, `MarkRunning`, `MarkDone`, `MarkFailed`, `MarkCancelled`, `SetPID`, `SetPISession`. Unit tests via the existing doltlite test helper. |
| **D3** | **pi runner** | `internal/daemon/pi/` spawns `pi --mode rpc`, sends a prompt, parses events, detects end-of-turn, supports clean shutdown and SIGTERM/SIGKILL. Black-box tested with a fake pi (a tiny Go binary that emits canned RPC frames). |
| **D4** | **Scheduler** | In-memory queue + N workers consuming from a channel; integrates with DaemonStore for transitions; restart recovery sweep at startup. Tests with a mock runner. |
| **D5** | **HTTP API: submit / get / list / cancel / healthz / version** | Real handlers wired to scheduler + store. Integration test: spawn daemon in-proc, POST a job that runs the fake pi end-to-end, observe terminal `done`. |
| **D6** | **Messages API: tail + full** | Projection of `session.jsonl` to normalised event shape. Tested with a fixture jsonl file. |
| **D7** | **SSE stream** | `/v1/jobs/{id}/stream` fans out events to subscribers; last-N replay on connect. Tested with `httptest` + concurrent reader. |
| **D8** | **autosk integration: auto-claim + closure verification + kickback** | Worker hooks: snapshot `blocked_by` and call `Claim` on start; after each end-of-turn run `verifyClosure` (see §6.1.1) and, on invalid closure, send a corrective message back to the same pi session up to `max_corrections` times. Persist `closure_kind` on success and `corrections_used` throughout. The daemon **never** calls `autosk done` — that is the agent's job. Unit-tested with the existing task store and a fake pi runner. |
| **D9** | **CLI clients** | `autosk daemon submit/status/messages/cancel/list` go through HTTP; golden tests for output formatting (table + JSON). |
| **D10** | **Docs + AGENTS.md update + sample workflow** | `docs/daemon.md` quickstart; `AGENTS.md` gains an "if a daemon is running, prefer `autosk daemon submit` over manual shelling out" section; `README.md` mentions the new mode. |

Acceptance scenario (end-to-end, runs at the end of D8):

```
autosk daemon serve --workers 1 &
id=$(autosk create "Bump version to 0.2" -p 1 --json | jq -r .id)
job=$(curl -s -X POST localhost:7878/v1/jobs \
        -d "{\"task_id\":\"$id\",\"model\":\"sonnet:medium\"}" \
        | jq -r .job_id)
# poll
while [ "$(curl -s localhost:7878/v1/jobs/$job | jq -r .status)" != "done" ]; do sleep 2; done
autosk show "$id"        # status: done
curl -s localhost:7878/v1/jobs/$job/messages?limit=10
```

---

## 10. Explicit non-goals (post-v0)

- Multi-tenant / multi-user. One token, one project, one machine.
- Multi-host workers / federation.
- Re-attach to surviving pi processes after a daemon restart.
- Re-running a failed job (`POST /v1/jobs/{id}/retry`). Trivial follow-up.
- Persisted SSE replay older than what's in `session.jsonl`.
- Web UI / TUI dashboard.
- Pluggable runner (`pi` replaced by another agent).
- Workflow constraints around autosk status transitions during a run
  (e.g. forbid manual `done` while a job is running).
- Resource limits per job (CPU/mem/timeout besides `idle_timeout`).
- Cron / scheduled jobs.

These are simple to add once §4–§9 land; none of them change the API shape.

---

## 11. Risks & open questions

| Risk | Mitigation |
|---|---|
| pi's RPC schema may shift between versions. | D0 captures the exact events we depend on; the projection layer (`pi/events.go`) is the only place that needs to update. Unknown events degrade to `kind:"other"`. |
| Agent ignores corrective messages and keeps idling. | `max_corrections` (default 3) caps the loop; `idle_timeout` caps each individual turn. Both surface as `failed` with distinct `error` codes. |
| Closure verification races with a concurrent human running `autosk done` on the same task. | The check is read-then-classify; whichever caller closed the task wins, and the run reports the observed `closure_kind`. We do not try to attribute closure to the agent vs a human in v0. |
| Existing blockers list races with the agent adding/removing blockers mid-run. | `pre_blocked_by` is a one-shot snapshot taken before the first prompt is sent. "Decomposed" means at least one blocker exists today that did not exist then; removals are ignored. |
| Concurrent daemon-spawned pi runs in the same `cwd` will race on files. | Document the risk in `docs/daemon.md`; recommend `cwd` per request when running >1 worker; explicit "git worktree per job" is a post-v0 feature. |
| autosk DB file lock contention between the daemon and ad-hoc `autosk` CLI calls. | Already handled by the existing single-writer lock (`database is locked` retried). Workers serialize DB writes; pi child processes do not write to autosk. |
| `daemon_runs.session_path` becomes stale after the user nukes `.autosk/sessions/`. | `messages` endpoint returns `410 Gone` with `error:"session_missing"`. |
| Bearer token in a file means anyone with FS access can read it. | This is the same trust boundary as `.autosk/db` itself. Documented in `docs/daemon.md`. |
| Long prompts in `daemon_runs.prompt` blow up table size. | Acceptable for v0; revisit if a single run's prompt routinely exceeds 10 KB. |
| **Open:** the exact JSON shape pi emits for "turn finished" must be confirmed in D0. | Block D3 on D0. The plan does not commit to a specific marker yet. |
| **Open:** whether `pi --mode rpc` accepts user messages on stdin as JSON-RPC requests or as a different shape. | Same as above; D0 must answer. |

---

## 12. Layout summary

```
cmd/autosk/
  daemon.go                  # `autosk daemon` cobra wiring (serve / submit / status / …)
internal/
  daemon/
    config.go                # flags → Config struct
    server.go                # http.Server, routes, middleware, auth
    api.go                   # request/response types, marshalling, validation
    scheduler.go             # queue + worker pool, restart recovery
    job.go                   # Job, status transitions, prompt rendering from task
    sse.go                   # SSE hub + per-job fan-out
    pi/
      runner.go              # spawn, send, wait, terminate
      events.go              # raw stdout → normalised event projection
      transcript.go          # session.jsonl reader (tail + full)
    store/
      store.go               # DaemonStore interface
      sqlite.go              # doltlite-backed implementation, reusing store/doltlite tx helpers
  migrations/
    002_daemon_runs.sql
docs/
  daemon.md                  # quickstart, configuration, security caveats
  notes/
    pi-rpc-contract.md       # D0 reconnaissance output
scripts/
  pi-rpc-probe.sh            # repro of every pi RPC event kind we project
```

---

## 13. Tracking

Per AGENTS.md, every phase in §9 is one autosk task. Dependencies are
expressed via `autosk block` edges; the umbrella task `Daemon orchestrator
v0` is blocked by all of D0…D10 and represents "the feature is shippable".

The umbrella task and its subtasks are created from this plan; their ids
appear in the PR that lands the plan.
