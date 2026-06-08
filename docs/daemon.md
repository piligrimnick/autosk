# autoskd — the Rust daemon (sole owner of `.autosk/db`)

`autoskd` is a native Rust daemon that drives autosk tasks through their
workflows and is the **single process that opens `.autosk/db`** — the only
reader and writer of doltlite. Every front end (the `autosk` CLI, the `autosk
lazy` TUI, the Tauri desktop GUI, and remote/mobile clients) talks to it over
one **JSON-RPC** protocol. **One daemon per host serves any number of
projects** from a single process; the project is selected per request via a
`{cwd}` (or `{db_path}`) selector on every method.

For each in-flight task it picks up, `autoskd` resolves the current step's
**agent package** (an npm package installed via `autosk agent install`) and
dispatches to one of two branches:

- **Standard branch** — the package declares `model` / `thinking` /
  `first_message` / etc. but no `runner`. The executor spawns `pi --mode rpc`
  with those settings and waits for the agent to call `autosk step next`. On a
  missed turn it kicks back up to `max_corrections` times before failing.

- **Custom branch** — the package declares an `autosk.agent.runner` path. The
  executor spawns the Node bootstrapper (`@autosk/agent-runtime`, installed in
  `~/.autosk/packages/`), feeds it a JSON `RunContextSeed` on stdin, and waits
  for the process to exit. Custom runners are single-shot — there is no
  kickback. They emit `autosk step next` via `ctx.cli(...)` (or
  `ctx.stepNext(...)`) inside the runner module; the executor observes
  `step_signals` exactly like the standard branch.

Architecture reference:
[`docs/plans/20260607-Rust-Daemon-Tauri-GUI.md`](plans/20260607-Rust-Daemon-Tauri-GUI.md).
The domain (store/doltlite, migrations, workflow engine, executor, poller,
scheduler, compactor, projectmgr, registry) lives in the `autosk-core` crate;
the JSON-RPC server lives in the `autoskd` crate.

---

## Quickstart

```bash
# 0. Have one or more project roots with .autosk/ initialised:
cd ~/work/project-a && autosk init        # one-time per project
cd ~/work/project-b && autosk init

# 1. There is NO manual "start the daemon" step for local use. The first
#    autosk CLI/lazy/GUI call that needs the daemon auto-spawns autoskd
#    (language-server style) and connects to it.

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

# 6. Cancel.
autosk daemon cancel "$JOB"
```

There is no `autosk daemon submit` command. Work enters the daemon through
workflow enrolment; the per-project poller (default cadence 2s) surfaces it
from `daemon_runs`.

### Running the daemon explicitly

Local clients auto-spawn `autoskd`, so you rarely start it by hand. When you do
want a foreground daemon (debugging, or a remote host that can't be
auto-spawned), run the `autoskd` binary directly:

```bash
autoskd                       # serve on the default socket (== `autoskd serve`)
autoskd serve --sock /tmp/a.sock
autoskd serve --tcp 0.0.0.0:7878   # opt-in remote transport (token auth)
autoskd init   ~/work/project-a    # greenfield: create + migrate .autosk/db
autoskd version
autoskd engine                # print the linked doltlite engine (link smoke test)
```

The Go `autosk daemon serve` verb is **retired** — `autoskd` is the daemon now.
The `autosk daemon status | messages | cancel | list` subcommands are JSON-RPC
clients of `autoskd` and auto-spawn it on first use.

---

## Auto-spawn lifecycle (local)

The CLI and lazy share a language-server-style connector
(`internal/daemon/rpcclient`); the Tauri GUI's local mode mirrors it in Rust:

1. Resolve the UDS path: explicit `--sock` → `$AUTOSK_SOCK` →
   `~/.autosk/daemon.sock`.
2. Try to connect. Success → use it.
3. On a missing / stale socket → locate the `autoskd` binary (explicit override
   → `$AUTOSKD_BIN` → alongside the calling binary → `PATH`) and spawn it
   detached (`autoskd serve --sock <path>`, new session, stdio to `/dev/null`).
4. `autoskd` performs **single-instance** binding. If two clients race, the
   loser detects "already running", exits `0`, and connects to the winner.
5. The client waits for readiness (connect with bounded backoff, ~5s) and
   proceeds.

Remote (`--tcp`) daemons cannot be auto-spawned — you can't fork a process on
another host — so a remote host runs `autoskd` as an explicit service.

### Auto-init in a fresh directory

`autosk create` in a directory with no `.autosk/db` keeps the interactive y/n
TTY prompt (and the `AUTOSK_AUTOINIT_*` / `AUTOSK_NO_AUTOINIT` knobs)
**client-side**, then calls `project.init {cwd}` on `autoskd`, which runs the
migrations + bootstrap before the `task.create`. The DB is still only ever
created and migrated by `autoskd`.

---

## Configuration

`autoskd serve` is CLI-flag-only (plus a few env knobs). Local clients spawn it
with `serve --sock <path>` and otherwise rely on the defaults.

| Flag | Default | Effect |
|---|---|---|
| `--sock` | `~/.autosk/daemon.sock` (env `AUTOSK_SOCK`) | Unix-domain socket path. Parent dir is created `0700`, socket `0600`. |
| `--tcp` | unset | Opt-in remote transport: bind a TCP listener on `HOST:PORT` with token auth. When set, idle-shutdown is disabled (a remote daemon is a long-lived service). |
| `--workers` | `4` | Max concurrent agent processes **across all projects** (single FIFO queue). |
| `--gc-interval` | `30m` (`0` / negative disables) | How often each project's compactor runs `SELECT dolt_gc()` against its `.autosk/db`. See [§ Compactor & the closed GC race](#compactor--the-closed-gc-race). |
| `--pi-bin` | `pi` | pi binary to spawn (looked up on `PATH` unless absolute). |
| `--no-exec` | off | Serve reads+writes but never auto-dispatch workflow steps (test affordance; env `AUTOSK_NO_EXEC=1`). Never set in production. |

The per-project workflow scan / change-notification cadence is a fixed internal
default of `2s` (`DaemonConfig.poll_interval`); it has no CLI flag.

Env knobs:

| Env | Effect |
|---|---|
| `AUTOSK_SOCK` | Default socket path (overridden by `--sock`). |
| `AUTOSK_IDLE_SECS` | Idle-shutdown window in seconds (default `1800`; `0` disables). See [§ Idle-shutdown](#idle-shutdown). |
| `AUTOSK_TOKEN_FILE` | Override the TCP token path (default `~/.autosk/daemon-token`). |
| `AUTOSKD_BIN` | Explicit `autoskd` binary the client connector spawns. |
| `AUTOSK_NO_EXEC` | `1`/`true` ⇒ `--no-exec`. |

`AUTOSK_DB` is **client-side only** — the daemon never consults it. Every
request resolves a project via its `{cwd}` (+ optional `{db_path}`) selector,
which is what lets one daemon serve many projects safely.

---

## Projects: registry + walk-up resolution

`autoskd` learns about a project two ways:

- **Persisted registry.** `~/.autosk/projects.json` (atomic write, file `0600`,
  dir `0700`) is the durable list of known projects, surfaced by
  `project.list` / `project.add` / `project.remove`. It is the GUI sidebar's
  source of truth. `autoskd init` and `project.init` register the project they
  create.
- **Walk-up resolution.** For CLI ergonomics, any method's `{cwd}` selector is
  resolved by walking up from `cwd` to the nearest `.autosk/db` (an optional
  `{db_path}` overrides the walk-up). This keeps the standalone CLI working in a
  directory that was never explicitly registered.

Projects are opened **lazily** on the first request that names them and stay
resident until the daemon exits. On first open, each project starts its
per-project **poller** (workflow scan), **compactor** (GC), and
**change-poller** (notification source). Rows whose `status='running'` at
first-open are rewritten to `failed` with `error='daemon_restart'`.

---

## JSON-RPC protocol

One JSON object per line, over UDS (local) and TCP (remote):

- Request: `{"id":<u64>,"method":"<string>","params":<object|null>}`
- Response: `{"id":<u64>,"result":<any>}` or `{"id":<u64>,"error":{"code","message","details"}}`
- Notification (server→client): `{"method":"<string>","params":<object>}`

Result shapes are serde mirrors of the wire types in the `autosk-proto` crate,
RFC3339 UTC for every timestamp (the machine-wire-format rule from
`AGENTS.md`). The Go client and the Tauri client deserialise the same types.

### Methods

| Domain | Methods |
|---|---|
| meta | `version`, `auth`, `healthz` (`{all?}`), `shutdown` |
| project | `project.list`, `project.add`, `project.remove`, `project.init`, `project.subscribe` / `project.unsubscribe` |
| task | `task.list` (filter), `task.get`, `task.ready`, `task.create`, `task.update`, `task.done`, `task.cancel`, `task.reopen`, `task.setStatus`, `task.setTitleDescription`, `task.setPriority`, `task.enroll`, `task.resume`, `task.block`, `task.unblock`, `task.unblockAll`, `task.subscribe` / `task.unsubscribe` |
| comment | `comment.add`, `comment.list` |
| workflow | `workflow.list`, `workflow.get`, `workflow.create`, `workflow.delete`, `workflow.updateIsolation` |
| agent | `agent.list`, `agent.install`, `agent.uninstall` |
| job | `job.list` (filter), `job.get`, `job.cancel`, `job.messages` (`{full,limit}`), `job.input` (`{message,behavior}`), `job.abort`, `job.subscribe`, `job.unsubscribe` |
| signal | `signal.forTask`, `signal.forJob` |
| sql | `sql.query`, `sql.exec` (raw passthrough for `autosk sql`) |
| step | `step.next` (record a workflow transition) |
| maint | `maint.compact` (force a GC pass) |

`version` and `healthz {all:true}` are exempt from the project selector;
everything else carries `{cwd}` (or `{db_path}`).

### Change notifications & streaming

`autoskd` pushes notifications instead of HTTP/SSE:

- **`task-changed` / `project-changed`** — a client subscribes with
  `task.subscribe` / `project.subscribe`; the daemon pushes whenever a task or
  project mutates. These are eager after a successful write **and** poll-backed
  by the per-project change-poller, so they also cover the daemon's own
  executor-driven advances. They replace lazy's old client-side 2s poll with a
  server push.
- **`job-event`** — a client calls `job.subscribe {jobId, attach?, full?,
  limit?, fromEventId?}` and receives `job-event` notifications
  (`{kind:"message"|"status"|"done"|"error", …}`) until it calls
  `job.unsubscribe` or disconnects. `fromEventId` / `limit` / `full` give
  replay-then-tail (the old `Last-Event-ID` semantics); `attach:true` increments
  the per-job attach counter that disarms the executor's idle-timeout.

---

## Security model

- **UDS auth = filesystem permissions.** The socket is `0600`, its parent
  directory `0700`. Anyone able to read `~/.autosk/` already has full
  read/write access to your project DB(s), so there is no extra trust boundary
  to defend with tokens locally. UDS connections are exempt from the auth
  handshake.
- **TCP auth = token.** The remote transport is opt-in (`--tcp HOST:PORT`).
  Over TCP the **first request must be `auth{token}`**; until it succeeds only
  `auth` is served. The token lives at `~/.autosk/daemon-token` (`0600`,
  override with `$AUTOSK_TOKEN_FILE`) and is minted from `/dev/urandom` on the
  first `serve` if absent.
- **Tool access is pi's.** The spawned pi (or custom runner) inherits the
  daemon's environment and can shell, edit files, install dependencies, etc. Do
  not point a project's cwd at directories you would not give an interactive pi
  session.
- **Concurrent runs in the same project may race on files.** The global worker
  pool serialises across projects but does not prevent two jobs in the same
  project from touching the same path. Mark a workflow with
  `"isolation": "worktree"` (see
  [`docs/workflows.md` § Worktree isolation](workflows.md#worktree-isolation))
  to give each task its own git worktree on its own branch; the daemon then
  spawns step runs with `cwd` pointing at the worktree and threads `AUTOSK_DB`
  so `autosk` CLI calls inside the worktree still find the canonical project DB.

---

## Idle-shutdown

Because `autoskd` drives `work` tasks autonomously via the poller, it is not
purely client-scoped. Idle-shutdown fires only when **all** of these hold past
the idle window (default 30 min, env `AUTOSK_IDLE_SECS`, `0` disables):

- no live client connections (UDS **and** TCP — every connection counts, not
  just notification subscribers);
- no queued or running jobs;
- no non-terminal `status='work'` tasks across loaded projects.

A watchdog re-checks every 10s. When it shuts down, the next client
transparently respawns the daemon. The `shutdown` RPC forces an immediate exit
(it tears down the projects, releases the UDS, and exits the process). Idle-
shutdown is disabled entirely in TCP-service mode (`--tcp`).

---

## Compactor & the closed GC race

Each loaded project runs a background **compactor** that ticks every
`--gc-interval` (default 30m) and invokes `SELECT dolt_gc()` to reclaim stale
chunks. GC is what keeps `.autosk/db` queries fast over the long haul.

doltlite implements GC via *write-to-sidecar + atomic rename*, so the on-disk
inode of `.autosk/db` rotates on every successful run. With the old Go stack —
where many processes opened the DB through a cross-process driver — this left a
~10⁻⁷ mid-statement race where an in-flight write could land in the orphan
inode and disappear.

**That race is gone.** With `autoskd` as the sole owner of the DB, the compactor
takes an in-process **write lock** and every query holds a **read lock** for its
full duration, so GC simply waits for in-flight queries to drain before it
rotates the inode. The Rust side links doltlite **0.11.8**, where the GC
regressions that forced the Go 0.10.8 pin are fixed (the 0.10.11 schema-cookie
corruption was fixed in 0.11.2).

---

## Closure verification

After each agent turn the executor classifies the workflow step's outcome via
`step_signals`:

| Verdict | Condition | Action |
|---|---|---|
| `transition_emitted` | the step's runner called `autosk step next` | Run terminates as `done`, task advances. |
| (none) | no signal observed | The daemon sends a corrective user message; `corrections_used += 1`. After `max_corrections` the run terminates as `failed` with `error="agent_did_not_emit_transition"`. |

The daemon **never** calls `autosk done`/`cancel` directly — that is owned by
the runner via `step next`.

The recorded `step_signal` is the source of truth for the turn outcome. If the
agent wrote a signal but the executor's wait on the `agent_end` event then
errored (pi pipe died, the extension-RPC payload broke the reader, an
unattached idle-timeout fired before `agent_end` landed, …), the executor still
honours the signal and advances the task instead of parking it. The original
error is preserved in the daemon log as `executor: wait_for_agent_end errored
but step_signal already recorded; honoring signal` so the misbehaving turn is
still investigable. Cancellation is excluded from this recovery path and routes
through the normal cancel-aware cleanup.

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `autoskd binary not found` | The connector could not locate `autoskd`. Build it (`make build-autoskd`) and/or set `$AUTOSKD_BIN`, or put it on `PATH`. |
| `autoskd did not become ready at <sock>` | Spawn raced or the socket path is wrong. Check `$AUTOSK_SOCK`; remove a stale socket; inspect a foreground `autoskd serve`. |
| `autoskd: already running at <sock>` | Single-instance: another `autoskd` owns the socket. Harmless during auto-spawn (the loser exits `0`). |
| Run sits in `running` forever | The agent never emits `agent_end`. The executor fails it after the idle-timeout — unless the agent already wrote a `step_signal`, in which case the recorded transition is honoured and the task advances (see [Closure verification](#closure-verification)). |
| Daemon log: `executor: wait_for_agent_end errored but step_signal already recorded; honoring signal` | The turn finished correctly from the agent's side (the `step_signal` is in the DB) but the reader / idle-timer tripped during shutdown. The run is recorded as `done` and the task advances; the log line is the forensic breadcrumb (inspect `err=`). |
| Run fails with `agent_did_not_emit_transition` | The agent stopped without calling `autosk step next`, `max_corrections` times. Inspect the transcript via `autosk daemon messages`. |
| Run fails with `daemon_restart` | The daemon restarted while this run was active; this iteration does not re-attach. Re-enroll the task. |
| Daemon log: `executor: re-allocated missing worktree` | An isolated workflow's per-task worktree directory was gone at run start; the executor re-allocated it on the existing branch (`autosk/<task-id>`) and continued. No human action required. |
| Run fails with `worktree_stranded` | An isolated workflow's worktree directory exists but its `.git` no longer resolves to the project's gitdir (typical when the project was moved or re-initialised). The task is parked → `human`. See [`docs/workflows.md` § Recovering from `worktree_stranded`](workflows.md#recovering-from-worktree_stranded). |
| TCP client gets `auth required` | The first request over TCP must be `auth{token}`. Read the token from `~/.autosk/daemon-token` (or `$AUTOSK_TOKEN_FILE`). |
| TCP client gets `invalid or missing token` | The supplied token does not match the daemon's. Copy the current `~/.autosk/daemon-token` from the daemon host. |

---

## References

- Architecture plan: [`docs/plans/20260607-Rust-Daemon-Tauri-GUI.md`](plans/20260607-Rust-Daemon-Tauri-GUI.md)
- TUI client: [`docs/lazy.md`](lazy.md)
- Desktop GUI client: [`gui/README.md`](../gui/README.md)
- Workflows / engine: [`docs/workflows.md`](workflows.md)
</content>
</invoke>
