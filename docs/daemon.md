# autosk daemon — multi-project pi orchestrator

`autosk daemon` is an HTTP-over-UDS service that drives autosk tasks
through their workflows. **One daemon per host** serves **any number
of projects** from a single process. The project is selected per
request via `X-Autosk-Cwd` / `X-Autosk-DB` headers.

For each in-flight task it picks up, the daemon resolves the current
step's **agent package** (an npm package installed via `autosk agent
install`) and dispatches to one of two branches:

- **Standard branch** — the package declares `model` / `thinking` /
  `first_message` / etc. but no `runner`. The executor spawns
  `pi --mode rpc` with those settings and waits for the agent to call
  `autosk step next`. On a missed turn it kicks back up to
  `max_corrections` times before failing.

- **Custom branch** — the package declares an `autosk.agent.runner`
  path. The executor spawns the Node bootstrapper
  (`@autosk/agent-runtime`, installed in `~/.autosk/packages/`), feeds
  it a JSON `RunContextSeed` on stdin, and waits for the process to
  exit. Custom runners are single-shot — there is no kickback. They
  emit `autosk step next` via `ctx.cli(...)` (or `ctx.stepNext(...)`)
  inside the runner module; the executor observes `step_signals`
  exactly like the standard branch.

Multi-project plan:
[`docs/plans/20260518-Daemon-UDS-Plan.md`](plans/20260518-Daemon-UDS-Plan.md).
Agent packages plan:
[`docs/plans/20260518-Agent-Packages.md`](plans/20260518-Agent-Packages.md).
Daemon foundations:
[`docs/plans/20260517-Daemon-Plan.md`](plans/20260517-Daemon-Plan.md)
(transport/auth/scope sections are superseded by the UDS plan).

---

## Quickstart

```bash
# 0. Have one or more project roots with .autosk/ initialised:
cd ~/work/project-a && autosk init        # one-time per project
cd ~/work/project-b && autosk init

# 1. Launch the daemon. Default socket = ~/.autosk/daemon.sock, workers = 2.
autosk daemon serve &

# 2. Enroll a task into a workflow from any project root. The per-project
#    poller picks it up automatically.
cd ~/work/project-a
id=$(autosk create "Tidy README" -p 1 --json | jq -r .id)
autosk enroll "$id" --workflow feature-dev

# 3. Inspect what the daemon is doing for *this* project:
autosk daemon list

# 4. See every project the daemon currently has loaded:
autosk daemon list --all-projects

# 5. Tail a job (cwd-scoped):
autosk daemon status   "$JOB"
autosk daemon messages "$JOB" --limit 20

# 6. SSE stream (raw curl over the socket):
curl -N --unix-socket "$HOME/.autosk/daemon.sock" \
     -H "X-Autosk-Cwd: $PWD" \
     "http://autosk/v1/jobs/$JOB/stream"

# 7. Cancel.
autosk daemon cancel "$JOB"
```

There is no `autosk daemon submit` command and no `POST /v1/jobs`
route. Work enters the daemon through workflow enrolment; the poller
(default cadence 2s, per project) surfaces it from `daemon_runs`.

---

## Configuration

`autosk daemon serve` is CLI-flag-only.

| Flag | Default | Effect |
|---|---|---|
| `--sock` | `~/.autosk/daemon.sock` (env `AUTOSK_SOCK`) | Unix-domain socket path. Parent dir is created with mode `0700`, socket with mode `0600`. |
| `--workers` | `2` | Max concurrent agent processes **across all projects** (single FIFO queue). |
| `--grace` | `10s` | Time SIGTERM has to bring the agent down before SIGKILL. |
| `--idle-timeout` | `30m` | Max time between agent events on a single turn before failing the run. |
| `--poll-interval` | `2s` | Per-project workflow scan cadence. |
| `--pi-bin` | `pi` | pi binary to spawn (looked up on PATH unless absolute). |
| `--session-dir-root` | `<projectRoot>/.autosk/sessions` when unset | Literal parent dir for per-job session subdirs. When set, the same path is shared across **all** projects served by this daemon; when empty (the default) each project gets its own `<projectRoot>/.autosk/sessions`. No template substitution is performed. |
| `--gc-interval` | `30m` (0 == default, negative disables) | How often each project's compactor runs `SELECT dolt_gc()` against its `.autosk/db`. See [§ Compactor & cross-process freshness](#compactor--cross-process-freshness). |

Project state lives in each project's `.autosk/db`. Projects are
opened **lazily** on the first request that names them and stay
resident until the daemon exits. Rows whose `status='running'` at
first-open are rewritten to `failed` with `error='daemon_restart'`
(per project, once per daemon lifetime).

### Single-instance guarantee

`daemon serve` refuses to start if another live daemon already owns
the socket:

```
$ autosk daemon serve
autosk: uds: daemon already running at /Users/me/.autosk/daemon.sock
```

If the socket file is *stale* (no peer accepts connections — typical
after a crash), the daemon unlinks it and rebinds.

### Compactor & cross-process freshness

Each loaded project runs a background **compactor** that ticks every
`--gc-interval` (default 30m) and invokes `SELECT dolt_gc()` to
reclaim stale chunks. GC is what keeps `.autosk/db` queries fast over
the long haul, but it has one subtlety operators should know about:

Doltlite implements GC via *write-to-sidecar + atomic rename*, so the
on-disk inode of `.autosk/db` changes on every successful run. Any
process whose connection was open at gc time keeps its file
descriptor pointing at the *orphan* inode. Without intervention a
reader would serve the pre-gc snapshot forever; a writer would route
its INSERT/COMMIT into the orphan where it disappears the moment the
conn closes — silent data loss.

The defence lives in `internal/store/doltlite/driver.go`: every
doltlite store opens through an inode-validating wrapper around
mattn/go-sqlite3. On every pool checkout the wrapper stats the path
and compares against the inode the conn was opened against; on a
mismatch it returns `driver.ErrBadConn` so `database/sql` retires the
conn and opens a fresh one. `autosk lazy` additionally rotates conns
on `--refresh` (belt-and-suspenders) and exposes `Ctrl-R` as a manual
hard-refresh; see
[`docs/lazy.md` § Cross-process freshness](lazy.md#cross-process-freshness).

Residual race: a gc that completes *mid-statement* or *mid-transaction*
on a writer's conn can still lose the in-flight bytes because the
validator runs only between pool checkouts. Per-write probability is
on the order of 10⁻⁷ with default settings; closing it would require
a cross-process advisory lock (compactor exclusive, writers shared)
that both the daemon and ad-hoc CLI invocations respect.

### The daemon ignores its own AUTOSK_DB

`AUTOSK_DB` is **client-side only**. The daemon process itself never
consults the env var; every request resolves a project via
`X-Autosk-Cwd` + optional `X-Autosk-DB`. This is what lets one daemon
serve many projects safely.

---

## HTTP API (over unix-domain socket)

The wire shape is plain HTTP/1.1 over `AF_UNIX`. Use `curl --unix-socket`
or, in code, an `http.Client` with `Transport.DialContext` returning a
`net.Dial("unix", path)` connection.

| Method | Path | Purpose |
|---|---|---|
| `GET`    | `/v1/jobs`                        | List jobs for the request's project. `?status=`, `?task_id=`, `?limit=`. |
| `GET`    | `/v1/jobs/{job_id}`               | Read one job (scoped to project). |
| `DELETE` | `/v1/jobs/{job_id}`               | Cancel (SIGTERM → grace → SIGKILL); idempotent on terminal rows. |
| `GET`    | `/v1/jobs/{job_id}/messages`      | `?limit=N` (≤500), `?full=true`. 410 Gone when session is missing. |
| `GET`    | `/v1/jobs/{job_id}/stream`        | SSE: `event: message`, `event: status`, `event: done`. Supports `Last-Event-ID`. |
| `GET`    | `/v1/healthz`                     | Scoped: `{ok, workers, queued, running, db_path, project_root}`. With `?all=true`: cross-project aggregate (no `X-Autosk-Cwd` required). |
| `GET`    | `/v1/version`                     | autosk build info. Exempt from project headers. |

### Required headers

Every endpoint except `GET /v1/version` and `GET /v1/healthz?all=true`
requires:

| Header | Required | Meaning |
|---|---|---|
| `X-Autosk-Cwd` | yes (absolute path) | Project root or any path inside it; the daemon walks up to find `.autosk/db`. |
| `X-Autosk-DB`  | optional (absolute path) | Overrides walk-up resolution. Wins over `X-Autosk-Cwd` for DB selection, but the project root is still derived from this DB path. |

Missing/malformed `X-Autosk-Cwd` → `400`. A `cwd` that does not
contain `.autosk/db` anywhere up the tree → `404`.

### POST `/v1/jobs` is gone

The previous v0.1 submit endpoint (which spawned ad-hoc pi sessions
with caller-supplied prompts) is removed. Submitting work is now
strictly:

```bash
autosk create   "..." --workflow feature-dev
autosk create   "..." --agent    @autosk/developer
autosk enroll   <id>  --workflow feature-dev
autosk enroll   <id>  --agent    @autosk/developer
```

…and the per-project poller picks it up.

---

## Security model

- **Auth = filesystem permissions.** The socket is `0600`, its
  parent directory is `0700`. Anyone able to read `~/.autosk/` already
  has full read/write access to your project DB(s), so there is no
  additional trust boundary to defend with tokens here.
- **No network exposure.** The daemon does not listen on TCP. There
  is no `--bind` flag, no `--token-file`, no Bearer auth. A future
  iteration will reintroduce a network mode behind a clearly distinct
  command surface.
- **Tool access is pi's.** The spawned pi (or custom runner)
  inherits the parent environment, can shell, edit files, install
  dependencies, etc. Do not point a project's cwd at directories you
  would not give an interactive pi session access to.
- **Concurrent runs in the same project may race on files.** The
  global worker pool serialises across projects but does not prevent
  two jobs in the same project from touching the same path. Mark a
  workflow with `"isolation": "worktree"` (see
  [`docs/workflows.md` § Worktree isolation](workflows.md#worktree-isolation))
  to give each task its own git worktree on its own branch; the
  daemon then spawns step runs with `cwd` pointing at the worktree
  and threads `AUTOSK_DB` so `autosk` CLI calls inside the worktree
  still find the canonical project DB.

---

## Closure verification

After every `agent_end` event the daemon classifies the workflow step's
outcome via `step_signals`:

| Verdict | Condition | Action |
|---|---|---|
| `transition_emitted` | the step's runner called `autosk step next` | Run terminates as `done`, task advances. |
| (none) | no signal observed | Daemon sends a corrective user message; `corrections_used += 1`. After `max_corrections` the run terminates as `failed` with `error="agent_did_not_emit_transition"`. |

The daemon **never** calls `autosk done`/`cancel` directly — that is
owned by the runner via `step next`.

The recorded `step_signal` is treated as the source of truth for the
turn outcome. If the agent wrote a signal but the executor's wait on
the `agent_end` event then errored — pi pipe died, extension-RPC
payload broke the reader, the unattached `--idle-timeout` fired before
`agent_end` landed, etc. — the executor still honours the signal and
advances the task instead of parking it. The original error is
preserved in the daemon log as
`executor: WaitForAgentEnd errored but step_signal already recorded; honoring signal`
so the misbehaving turn is still investigable. Cancellation
(`ctx.Err() != nil` or `errors.Is(err, context.Canceled)`) is excluded
from this recovery path and routes through the normal cancel-aware
cleanup.

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `daemon already running at <sock>` | Another `autosk daemon serve` owns the socket. `ps -fU $(whoami) \| grep autosk` to find it. |
| `400 "X-Autosk-Cwd header required"` | Client did not set the header. CLI subcommands set it automatically from `--cwd` or `os.Getwd()`. |
| `404 "no .autosk/db found from <cwd>"` | The cwd is outside any autosk project. Run `autosk init` or supply `--cwd` pointing into a real project. |
| Run sits in `running` forever | The agent never emits `agent_end`. The daemon will fail it after `--idle-timeout` — unless the agent already wrote a `step_signal`, in which case the recorded transition is honoured and the task advances (see [Closure verification](#closure-verification)). |
| Daemon log: `executor: WaitForAgentEnd errored but step_signal already recorded; honoring signal` | The turn finished correctly from the agent's side (the `step_signal` is in the DB) but the reader / idle-timer tripped during shutdown. The run is recorded as `done` and the task advances; the log line is the forensic breadcrumb. Inspect the `err=` field to see whether the reader died, the idle-timer fired, or another failure mode landed. |
| Run fails with `agent_did_not_emit_transition` | The agent stopped without calling `autosk step next`, `max_corrections` times. Inspect transcript via `autosk daemon messages`. |
| Run fails with `daemon_restart` | The daemon was restarted while this run was active. This iteration does not re-attach. Re-enroll the task. |
| Daemon log: `executor: re-allocated missing worktree` | Isolated workflow's per-task worktree directory was gone at run start; the executor re-allocated it on the existing branch (`autosk/<task-id>`) and continued the run. No human action required. The previous behaviour (park → `human`) only applies to `worktree_stranded` now. |
| Run fails with `worktree_stranded` | Isolated workflow's worktree directory exists but its `.git` no longer resolves to the project's gitdir (typical when the project was moved or re-initialised). Task is parked → `human`. See [`docs/workflows.md` § Recovering from `worktree_stranded`](workflows.md#recovering-from-worktree_stranded). |
| 410 on `/v1/jobs/{id}/messages` | `session_path` is empty or the file was deleted. |
| Stream connection drops | Long polls may need `X-Accel-Buffering: no` (already sent). Use `Last-Event-ID` on reconnect to skip replay. |

---

## Open items (next iterations)

- Reintroduce a remote HTTP API for network-accessible deployments.
- Idle-eviction of projects from memory.
- Per-project worker limits / priorities between projects.
- Multi-user / shared-host hardening (SO_PEERCRED).
- Explicit project registration (replaces the lazy walk-up from
  `X-Autosk-Cwd`; not related to the removed `autosk attach` CLI verb).
- Reconnect to surviving pi processes after a daemon restart.
- Cross-project enumeration for `autosk worktree list --all-projects`
  (per-project isolation already lands in v0.3 via the workflow
  `isolation: worktree` opt-in; see
  [`docs/workflows.md` § Worktree isolation](workflows.md#worktree-isolation)).

See [`docs/plans/20260518-Daemon-UDS-Plan.md`](plans/20260518-Daemon-UDS-Plan.md)
§10 for the canonical out-of-scope list.
