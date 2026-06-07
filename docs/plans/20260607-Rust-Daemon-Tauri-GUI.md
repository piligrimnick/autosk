# Rust daemon + Tauri GUI — architecture plan

Status: proposal (decisions locked, see §1). Supersedes the "future remote
HTTP API" open item in [`docs/daemon.md`](../daemon.md).

This plan introduces a graphical desktop front end for autosk modelled on
`~/me/dev/CodexMonitor` (Tauri: React/Vite UI + Rust backend), and a native
**Rust daemon** (`autoskd`) that becomes the **single owner of `.autosk/db`** —
the only process that reads or writes doltlite. Every other component (the
Tauri GUI, the Go CLI, the Go lazy TUI, remote/mobile clients) talks to
`autoskd` over one JSON-RPC protocol. This document is the canonical reference
for the cutover; the per-phase work breakdown in §9 is the implementation
contract.

---

## 1. Locked decisions

| Axis | Decision | Consequence |
|---|---|---|
| GUI shell | **Tauri** (Rust shell, React + Vite front) | IPC is Rust-centric; the Tauri backend is a thin JSON-RPC client of `autoskd` (and may embed it as a sidecar). |
| Daemon language | **Rust, native port (R1)** | The whole domain (store/doltlite, workflow engine, executor, poller) is reimplemented in Rust. |
| DB ownership | **`autoskd` is the sole reader+writer of `.autosk/db`** | No other process opens doltlite. Eliminates dual-domain drift; lets the GC race be fully closed in-process; strips CGO/doltlite from the Go binary. |
| Backend topology | **B — daemon = full backend + network mode** | `autoskd` exposes the entire domain over JSON-RPC on UDS (local) **and** TCP (remote, token auth). |
| Protocol | **Single JSON-RPC** (line-delimited JSON), streaming via server→client notifications | One coherent surface for CLI, lazy, GUI, remote. No HTTP/SSE compat face. |
| Lifecycle | **Auto-spawn on demand** (language-server style) for local; explicit service for remote hosts | Clients connect to the UDS and transparently spawn `autoskd` if absent; remote hosts run it explicitly. |
| Go daemon | **Retired** | `autosk daemon serve` (Go) is removed. |
| Go CLI + lazy TUI | **Kept, but rewired to pure JSON-RPC clients** | They lose direct DB access (`datasource.Offline` deleted) and **lose the CGO/doltlite link entirely**. |
| GUI scope v1 | **Parity with `autosk lazy`** | Tasks/jobs/workflows/agents CRUD + input/steer/follow_up/abort/cancel + live transcripts. No git-diff/PR extras in v1. |

### Why "sole owner" is the load-bearing decision

With `autoskd` as the only process touching doltlite:

- **No dual-domain drift.** The Go domain (`tasksvc`/`store`/`workflow`/
  `comments`/`agent`) is deleted, not duplicated. There is exactly one
  implementation of every invariant (the CHECK constraint, `step_visits`,
  worktree path derivation, DoltCommit messages). The expensive cross-language
  DB-state conformance suite the previous draft required is **no longer needed**.
- **The GC race closes completely.** Today doltlite's `dolt_gc()` (write-to-
  sidecar + atomic rename) rotates the on-disk inode, and the Go driver only
  *best-effort* revalidates per pool checkout, leaving a ~10⁻⁷ mid-statement
  race (`docs/daemon.md` § Compactor). With a single process owning the DB, the
  compactor takes an in-process **write lock** (RwLock) and every query holds a
  **read lock** for its full duration; GC simply waits for in-flight queries to
  drain. The race is gone, not merely shrunk.
- **The Go binary drops CGO.** CLI + lazy no longer link `libdoltlite.a`, no
  longer need `make doctor` / the doltlite fetch, and build with plain `go
  build`. Only `autoskd` (Rust) links doltlite.

### The new tension this introduces

There is no offline mode anymore — **CLI/lazy require `autoskd` to be
reachable**. The mitigation is the auto-spawn lifecycle (§4.2): a client that
finds no daemon starts one transparently, so the standalone CLI keeps working
"out of the box". Remote/mobile mode cannot auto-spawn (you can't fork a
process on another host) and therefore requires an explicitly-running
`autoskd` on the remote side.

---

## 2. Current state (what we are porting)

Researched from the Go tree; quoted so the Rust port has an exact target.

### 2.1 Data layer — doltlite

- `.autosk/db` is sqlite-on-doltlite. The **Go** side links `libdoltlite.a` +
  `sqlite3.h` via CGO + `mattn/go-sqlite3` with the `libsqlite3` build tag
  (see `Makefile`), pinned to **0.10.8** (0.10.9's per-ChunkStore
  `pthread_mutex` deadlocks against Go's goroutine/OS-thread migration; 0.10.11
  corrupted the schema cookie after `dolt_gc()`). The Go pin stays put until the
  Go binary loses doltlite entirely in Phase 3.
- The **Rust** side targets the current latest, **doltlite 0.11.8**. The GC
  regressions that forced the Go 0.10.8 pin were fixed in the 0.11.x line (e.g.
  PR #858 `fix/gc-rename-and-overflow`, `fix(gc): keep cs->pFile valid across
  rewrite`), so no multi-version investigation is needed; and Rust's 1:1
  threads make the 0.10.9 Go-specific deadlock irrelevant anyway. Phase 0 still
  verifies that 0.11.8's `dolt_gc` is corruption-free under the RwLock
  discipline and that a Go-0.10.8-created DB opens forward-compatibly under
  0.11.8 (the transition window has Go on 0.10.8 and Rust on 0.11.8 against the
  same on-disk format).
- Versioning is exposed as SQL functions: `SELECT dolt_commit(...)`,
  `SELECT dolt_gc()` (`internal/store/doltlite/commit.go`, `maint.go`).
- Schema/migrations: `internal/migrations/001_init.sql` + `migrations.go`.
  Tables: `tasks`, `task_deps`, `agents`, `workflows`, `steps`, `comments`,
  `daemon_runs`, `step_signals`.
- Key invariant: SQL CHECK ties `status='work'` ⇔ `current_step_id IS NOT NULL`.

### 2.2 Domain model

- Status enum (≤7 chars): `new | work | human | done | cancel`
  (`internal/store/store.go`). `blocked` is derived, not stored.
- Run status: `queued | running | done | failed | cancel`
  (`internal/daemon/runstore`).
- Task ids: `ask-XXXXXX` (`internal/id`).
- Task metadata: free-form JSON; engine reserves `step_visits`
  (`{step_id → uint}`) for `max_visits`.
- Worktree path is **derived, never stored**:
  `~/.autosk/worktrees/<basename>-<8hex(sha256(canonRoot))>/<task-id>`,
  branch `autosk/<task-id>` (`internal/worktree`).
- Agents are npm packages under `~/.autosk/packages`; config lives in
  `package.json#autosk.agent` (`model`/`thinking`/`first_message[_file]`/
  `extra_args`/`pi_extensions`/`pi_skills`/`runner`). `human` is the only
  non-package agent.

### 2.3 Workflow engine (`internal/workflow`)

Directed step graph; per-step `agent` + `max_visits` + `next_steps[]` (each
either a sibling `step` or a terminal `task_status`). `EnterStep` does the
atomic `step_visits` bump + pointer move + status flip
(`UpdateMetadataAndPatch`). Synthetic `single:<agent>` workflows for
`--agent`. Isolation `none|worktree`.

### 2.4 Daemon (`internal/daemon`) — the thing being replaced

- Transport: HTTP/1.1 over **AF_UNIX only**; no TCP, no auth; single-instance
  via socket probing (`internal/daemon/uds`).
- Project scoping: `X-Autosk-Cwd` (+ optional `X-Autosk-DB`) →
  `projectmgr.Resolve` walks up to `.autosk/db`, opens lazily, sweeps stale
  `running` → `failed/daemon_restart`. **No project registry.**
- HTTP surface (9 routes), run-control only — `internal/daemon/server`:
  `GET/DELETE /v1/jobs[/{id}]`, `/messages`, `/stream` (SSE), `POST .../input`,
  `POST .../abort`, `/healthz`, `/version`. **No task/workflow/comment writes**
  (those go through the store layer directly today).
- Wire types: `internal/daemon/api/types.go` (RFC3339 UTC). SSE frames:
  `event: message|status|done|error`, id-tagged, `Last-Event-ID` replay,
  `?attach=true` bumps the attach counter.
- Executor (`internal/daemon/executor`): resolves agent config, spawns
  `pi --mode rpc` (standard) or the Node `@autosk/agent-runtime` bootstrapper
  (custom), streams to `session.jsonl` + SSE, consumes `step_signals`, kickback
  ≤ `max_corrections`, disarms idle-timeout while attached, honours a recorded
  signal even if `WaitForAgentEnd` errored, worktree alloc/cleanup.
- pi wire (`internal/daemon/pi`): JSON-Lines over child stdin/stdout —
  `Command{id,type,message,streamingBehavior}` (`prompt|steer|follow_up|abort`),
  `Response{type:"response"}` ack, `Event` stream (`agent_end`, …).

### 2.5 The clean seam already in Go

`internal/lazy/datasource.Datasource` (~30 methods, TUI-agnostic) is the read+
write contract the lazy TUI talks through. In the new world its three
implementations (`Offline`/`Live`/`Compose`) **collapse into a single
JSON-RPC client implementation**. The interface itself is the **spec** for both
the Rust core's public surface and the JSON-RPC method list (§5) — reproduce
its method set verbatim so behaviour cannot drift.

---

## 3. Target topology

```
┌───────────────────────┐   ┌──────────────────────┐   ┌────────────────────┐
│ Tauri GUI (React/Vite)│   │ autosk CLI (Go)      │   │ autosk lazy (Go TUI)│
│ services/ipc.ts        │   │ pure JSON-RPC client │   │ Datasource = 1 RPC  │
│ services/events.ts     │   │ (no CGO/doltlite)    │   │ client impl         │
└──────────┬─────────────┘   └──────────┬───────────┘   └─────────┬──────────┘
           │ Tauri invoke/events         │ JSON-RPC                │ JSON-RPC
           │ (src-tauri thin adapter:    │ over UDS                │ over UDS
           │  local→client, remote→TCP)  │ (auto-spawn)            │ (auto-spawn)
           └───────────────┬─────────────┴────────────────────────┘
                           │  local: UDS   |   remote: TCP + token
                  ┌────────▼─────────────────────────────────────────┐
                  │ autoskd  (Rust) — SOLE owner of .autosk/db        │
                  │  crate autosk-core:                               │
                  │    store/    rusqlite → libdoltlite.a             │
                  │              + RwLock GC discipline (race-free)   │
                  │    domain/   tasks, deps, comments, agents, meta  │
                  │    engine/   workflow graph, transitions, visits  │
                  │    executor/ spawn pi --mode rpc / node runtime,  │
                  │              transcript, kickback, attach/input/  │
                  │              steer/abort, worktree isolation      │
                  │    poller / scheduler / compactor / projectmgr    │
                  │    registry (persisted project list)              │
                  │  rpc/       JSON-RPC server (UDS + TCP/token)      │
                  │  bus/       broadcast → job/task/project notifs    │
                  └────────────────────────┬─────────────────────────┘
                                           │ stdio JSON-Lines RPC (per step run)
                                  pi --mode rpc  /  @autosk/agent-runtime (Node)
                                           │ read+write
                                   .autosk/db (doltlite)   ← only autoskd opens it
```

### Cargo workspace

```
autosk/                 (Go module stays at root: CLI + lazy, now CGO-free)
  crates/
    autosk-core/        domain + store + engine + executor (the port)
    autosk-proto/       serde wire types + JSON-RPC method/notif definitions
    autoskd/            bin: the daemon; thin adapter over autosk-core
  gui/
    src/                React + Vite
    src-tauri/          Tauri app; thin adapter (local: in-proc client to a
                        co-located autoskd / sidecar; remote: TCP client)
```

`autosk-core` is the single Rust domain. `autoskd` and `src-tauri` are thin
adapters (CodexMonitor's "shared core + thin adapters"). `autosk-proto` is the
single source of truth for the wire contract, shared by `autoskd` and the
Tauri client; the Go client mirrors the same shapes.

---

## 4. Protocol & lifecycle

### 4.1 Single JSON-RPC surface

Transport: one JSON object per line, over UDS (local) and TCP (remote).
- Request: `{"id":<u64>,"method":"<string>","params":<object|null>}`
- Response: `{"id":<u64>,"result":<any>}` | `{"id":<u64>,"error":{"code","message","details"}}`
- Notification (server→client): `{"method":"<string>","params":<object>}`
- TCP auth handshake: first request must be `auth{token}` (UDS exempt — fs
  perms; socket `0600`, dir `0700`). Token at `~/.autosk/daemon-token` (`0600`).

Streaming replaces SSE: a client calls `job.subscribe {jobId, attach?, full?,
limit?, fromEventId?}` and receives `job-event` notifications (`{kind:
"message"|"status"|"done"|"error", …}`) until it calls `job.unsubscribe` or
disconnects. Replay-then-tail (`fromEventId`/`limit`/`full`) preserves the
current `Last-Event-ID` semantics; `attach:true` increments the per-job attach
counter that disarms the executor's idle-timeout.

Every method carries a project selector (`{cwd}` or `{dbPath}`), mirroring the
old `X-Autosk-Cwd`/`X-Autosk-DB` headers. Method names mirror the Go
`Datasource` interface. See §5 for the full list.

### 4.2 Auto-spawn lifecycle (local)

Language-server pattern, implemented in a shared Go connector (used by CLI +
lazy) and a Rust connector (used by `src-tauri` when co-located):

1. Resolve the UDS path (`$AUTOSK_SOCK` → `~/.autosk/daemon.sock`).
2. Try to connect. Success → use it.
3. On `ENOENT`/stale socket → locate the `autoskd` binary (PATH, then alongside
   the calling binary, then a configured path; the Tauri app ships it as a
   bundled sidecar) and spawn it detached.
4. `autoskd` performs single-instance binding (reuse the existing UDS probe
   semantics). If two clients race, the loser detects "already running" and
   connects to the winner.
5. Client waits for readiness (connect with bounded backoff) and proceeds.

**Shutdown policy.** `autoskd` is not purely client-scoped — it drives `work`
tasks autonomously via the poller. So idle-shutdown fires only when **all** of:
no connected clients, no running jobs, and no non-terminal `work` tasks across
loaded projects, persist past the idle window. `autosk daemon stop` (RPC
`shutdown`) forces it. Remote `autoskd` (TCP) never auto-shuts and is run as a
service.

**Auto-init under sole-ownership.** `autosk create` in a fresh dir: the CLI
keeps the interactive y/n TTY prompt and the `AUTOSK_AUTOINIT_*` /
`AUTOSK_NO_AUTOINIT` knobs client-side, then calls `project.init {cwd}` on
`autoskd` (which runs migrations + bootstrap) before `task.create`. The DB is
still only ever created/migrated by `autoskd`.

---

## 5. JSON-RPC method contract (v1)

Result shapes = serde mirrors of the existing `api.*`/`datasource.*` structs in
`autosk-proto`, RFC3339 UTC for all timestamps (the machine-wire-format rule
from `AGENTS.md`). The Go client and the Tauri client deserialise the same
types.

| Domain | Methods |
|---|---|
| meta | `version`, `auth`, `healthz` (`{all?}`), `shutdown` |
| project | `project.list`, `project.add`, `project.remove`, `project.init`, `project.subscribe` |
| task | `task.list` (filter), `task.get`, `task.ready`, `task.create`, `task.update` (title/desc/priority/status), `task.enroll`, `task.resume`, `task.block`, `task.unblock`, `task.metadata.set`, `task.subscribe` |
| comment | `comment.add`, `comment.list` |
| workflow | `workflow.list` (`{includeSynthetic}`), `workflow.get`, `workflow.create`, `workflow.delete`, `workflow.updateIsolation` |
| agent | `agent.list`, `agent.install`, `agent.uninstall` |
| job | `job.list` (filter), `job.get`, `job.cancel`, `job.messages` (`{full,limit}`), `job.input` (`{message,behavior}`), `job.abort`, `job.subscribe`, `job.unsubscribe` |
| signal | `signal.forTask`, `signal.forJob` |
| sql | `sql.query`, `sql.exec` (raw passthrough for `autosk sql`) |

Notifications: `job-event` (transcript/status/done/error, payload identical to
the old SSE frames), `task-changed`, `project-changed`. `task-changed` /
`project-changed` are poll-backed inside `autoskd` (it already polls projects);
they replace lazy's client-side 2s poll with a server push.

---

## 6. Tauri front end (mirror CodexMonitor)

### IPC discipline (one chokepoint each way)
- `gui/src/services/ipc.ts` — the only `invoke()` site; typed shims per command.
- `gui/src/services/events.ts` — the only `listen()` site; ref-counted fan-out
  hub so one Tauri listener serves N React subscribers.
- `gui/src-tauri/src/lib.rs` — `generate_handler![...]` registry. Each command:
  `if remote { autoskd_tcp_client.call(...) } else { autoskd_uds_client.call(...) }`.
  `autoskd` JSON-RPC notifications are re-emitted with `app.emit("<same>",
  payload)` so the React layer is transport-agnostic (CodexMonitor's key trick).
- Local mode ships `autoskd` as a **Tauri sidecar** the app starts/connects to;
  remote mode dials a user-configured `host:port` + token.

### UI regions (v1 = lazy parity, CodexMonitor layout)
- **Left sidebar** — projects (from the registry), each expandable into its
  tasks grouped by status, with running/streaming indicators (lazy Tasks + Jobs
  fused). Project add/remove/init.
- **Center "conversation" = task timeline** — the autosk-specific model: the
  task's jobs' transcripts concatenated chronologically, with comments and
  step-signals interleaved by timestamp, plus a live tail (`job.subscribe`) for
  the running job. `MessageEvent` rendering mirrors lazy's Detail pane
  (assistant_* via markdown).
- **Composer (state-aware)** — the delta from CodexMonitor's single box:
  - **running** job → `prompt`/`steer`, `follow_up`, `abort` (`job.input`/`job.abort`).
  - **human**-parked → add comment + `resume` (optionally `--to step`).
  - **new** → `enroll` (workflow/agent + step picker).
- **Right panel** — workflow steps (graph + per-step agent + `next=` targets),
  task metadata + visit counters, worktree path/branch. (Git-diff/PR view is
  post-v1.)
- **Secondary views** — Workflows (create from JSON, delete, isolation toggle),
  Agents (list; install/uninstall).

### State engine
CodexMonitor reducer pattern: a normalized, slice-composed reducer keyed by
project/task/job; the event router (`services/events.ts`) maps
`job-event`/`task-changed`/`project-changed` into reducer actions.

---

## 7. Delta vs today (what concretely changes)

### Rust (new)
1. **Port the whole domain** into `autosk-core`: store/doltlite, migrations,
   workflow engine, tasksvc-equivalent, comments, agents/pkgregistry, worktree,
   id, meta, plus the daemon's executor/poller/scheduler/compactor/projectmgr/
   transcript/pi-wire/runstore.
2. **doltlite from Rust** — `rusqlite`/`libsqlite3-sys` linked against
   `libdoltlite.a` for **doltlite 0.11.8** (build.rs: `rustc-link-lib=static=
   doltlite` + `-lz -lpthread -lm`, include `sqlite3.h`). Reuse
   `scripts/fetch-doltlite.sh 0.11.8 <dest>`. The Rust pin is fixed at 0.11.8
   (the Go-side GC regressions are fixed in 0.11.x — see §2.1); no version
   bake-off. Implement the **RwLock GC discipline** (compactor = write lock;
   queries = read lock) to close the GC race outright.
3. **JSON-RPC server** over UDS + TCP/token, with the §5 surface + `job/task/
   project` notifications + single-instance + auto-spawn readiness signalling +
   idle-shutdown policy (§4.2).
4. **Project registry** — persisted `~/.autosk/projects.json` (atomic, 0600)
   for the GUI sidebar (`project.add/remove/list`); lazy walk-up from `cwd`
   stays for CLI ergonomics.

### Go (rewire, then slim down)
5. **Delete `datasource.Offline`** and the `Live`/`Compose` split; replace with
   a **single JSON-RPC-client `Datasource` impl**. The lazy TUI is otherwise
   unchanged (it already talks only through the interface).
6. **Rewrite the CLI verbs** to route through `autoskd` (RPC) instead of opening
   the store: `create`/`enroll`/`resume`/`done`/`cancel`/`reopen`/`comment`/
   `metadata`/`workflow`/`agent`/`sql`/`init`/`ready`/`list`/`show`, plus
   `daemon status|messages|cancel|list` → the job RPCs.
7. **Strip CGO/doltlite from the Go build** — remove the `libsqlite3` build tag,
   the doltlite fetch from `make build`, and the store/doltlite packages used by
   the front ends. The Go binary builds with plain `go build`.
8. **Add the auto-spawn connector** (shared by CLI + lazy): connect-or-spawn
   `autoskd`, wait for readiness.

### Removed
9. The Go `autosk daemon serve` and `internal/daemon/*` server/executor.

### Explicitly unchanged
- `.autosk/db` schema, migrations content, DoltCommit messages, worktree path
  derivation, status enums, id format — now owned solely by `autosk-core`.
- The pi `--mode rpc` JSON-Lines protocol and `@autosk/agent-runtime`
  bootstrapper contract (agent side untouched; only the spawner is reimplemented).
- The pi extension / agent SDK (`extension/`).

---

## 8. Conformance / anti-drift

Dual-domain DB-state conformance is **no longer needed** (one writer). What
remains:

1. **Executor behavioural parity.** Reuse the Go fake-pi
   (`internal/daemon/pi/fakepi`) contract to drive the Rust executor through the
   same turn sequences — transition emitted / missed → kickback / idle-timeout /
   signal-honoured-after-reader-error / `max_corrections` fail — and assert
   identical `daemon_runs`/`step_signals` outcomes. This guards the trickiest
   ported logic.
2. **Protocol golden tests.** `autosk-proto` round-trip + golden JSON fixtures
   for every method/notification so the Go client and Tauri client never
   diverge from the server.
3. **Migration/schema golden.** Assert the Rust migrator produces the exact
   schema (`schema_version`, table DDL, CHECK) the Go `001_init.sql` produced —
   a one-time port-correctness gate, since old DBs must keep opening.
4. **Phase-1 dual-run safety net (temporary).** During the port, a read-only
   diff harness can point both the Go reader and `autosk-core` at the same
   fixture DB to catch porting bugs — retired once the Go store is deleted.

---

## 9. Phasing

- **Phase 0 — de-risk doltlite-from-Rust (spike).** `autosk-core::store` opens
  an existing `.autosk/db`, reads `tasks`, runs `dolt_commit`/`dolt_gc`, proves
  the RwLock discipline survives a GC under concurrent reads. Pin doltlite at
  **0.11.8** and verify its `dolt_gc` is corruption-free (the 0.10.11 cookie
  bug is fixed in 0.11.x) and that a Go-0.10.8 DB reads forward-compatibly under
  0.11.8. *Highest unknown — first.*
- **Phase 1 — read core + JSON-RPC reads.** Port store reads + migrations +
  projectmgr + registry; stand up the JSON-RPC server with the read methods +
  `job.subscribe` stub. Build the Go JSON-RPC client + auto-spawn connector;
  point a read-only lazy at `autoskd`.
- **Phase 2 — executor port.** pi JSON-Lines wire, transcript + `job-event`
  notifications, poller/scheduler/compactor, kickback, attach/input/steer/abort,
  worktree isolation. Executor behavioural-parity gate (§8.1). After this,
  `autoskd` drives workflows end-to-end → **retire the Go daemon**.
- **Phase 3 — write core + full RPC + Go rewire.** Port all writes into
  `autosk-core`; complete the §5 surface incl. `project.init`/`sql.*`/auth/TCP.
  Rewire CLI verbs + lazy writes to RPC; **delete `datasource.Offline` and the
  Go store/doltlite packages; strip CGO** (§7.5–7.9). Go now builds CGO-free.
- **Phase 4 — Tauri GUI v1 (lazy parity).** Cargo workspace + Vite scaffold;
  `ipc.ts`/`events.ts` chokepoints; sidecar `autoskd` (local) + TCP client
  (remote) + notification re-emission; sidebar + task-timeline + state-aware
  composer + workflows/agents views.
- **Phase 5 — cutover + docs.** Remove Go daemon remnants; rewrite
  `docs/daemon.md` (now `autoskd`, JSON-RPC, auto-spawn, registry, network
  mode), `docs/lazy.md` (RPC client, no offline mode), `README.md`,
  `AGENTS.md` (build no longer needs doltlite for the Go binary).

---

## 10. Open questions (decide during Phase 0–3)

- **doltlite format compat across the transition** — Go stays on 0.10.8 while
  Rust uses 0.11.8; Phase 0 verifies a 0.10.8-created DB reads cleanly under
  0.11.8. Bumping the Go side to 0.11.x is avoided (it may reintroduce the
  Go-thread-migration deadlock) since the Go binary loses doltlite in Phase 3.
- **Idle-shutdown tuning** — the "no clients AND no work AND no running jobs"
  window; whether the GUI sidecar should keep the daemon alive while open.
- **Remote filesystem semantics** — daemon host ≠ client host: worktree paths,
  `pi_extensions`/`pi_skills` paths, image/file attachments are host-relative.
  Needs a path/transfer policy (CodexMonitor normalises images to data URLs in
  remote mode).
- **Auth/key management** for TCP (token rotation, Tailscale) — follow
  CodexMonitor's `docs/mobile-ios-tailscale-blueprint.md` if mobile is pursued.
- **Conversation model edge cases** — multi-job timeline ordering when
  corrections/kickbacks interleave; rendering a parked task's "waiting for
  human" affordance in the composer.
- **CLI latency under auto-spawn** — first command pays the spawn+migrate cost;
  measure and, if needed, warm-start or persist the daemon more aggressively.

---

## 11. References

- Daemon (to be replaced): [`docs/daemon.md`](../daemon.md)
- TUI seam to mirror: [`docs/lazy.md`](../lazy.md), `internal/lazy/datasource`
- Workflows/engine: [`docs/workflows.md`](../workflows.md), `internal/workflow`
- doltlite linkage + pin rationale: `Makefile`, `internal/store/doltlite`
- Blueprint app: `~/me/dev/CodexMonitor` (Tauri shared-core + thin adapters,
  remote-backend notification re-emission, JSON-RPC daemon).
