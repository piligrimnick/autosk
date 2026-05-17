# autosk daemon — pi orchestrator

`autosk daemon` is an HTTP service that runs `pi --mode rpc` against
autosk tasks. It accepts a task id (or a raw prompt), spawns pi with the
requested model + thinking level, surfaces lifecycle / messages / SSE,
and verifies that the agent closes the task per protocol. The agent
itself is responsible for calling `autosk done` — the daemon only checks
and kicks back when the agent forgets.

Plan: [`docs/plans/20260517-Daemon-Plan.md`](plans/20260517-Daemon-Plan.md).
RPC contract notes: [`docs/notes/pi-rpc-contract.md`](notes/pi-rpc-contract.md).

---

## Quickstart

```bash
# 0. From your project root (must contain `.autosk/`):
autosk init                     # one-time, if not done already

# 1. Launch the daemon. Default bind = 127.0.0.1:7878, workers = 2.
autosk daemon serve &

# 2. Submit a task. Daemon renders the prompt from title + description.
id=$(autosk create "Tidy README" -p 1 --json | jq -r .id)
job=$(autosk daemon submit "$id" --model sonnet:medium --thinking high)

# 3. Watch status / messages.
autosk daemon status   "$job"
autosk daemon messages "$job" --limit 20

# 4. Stream live (raw curl example).
curl -N http://127.0.0.1:7878/v1/jobs/"$job"/stream

# 5. Cancel.
autosk daemon cancel "$job"
```

Ad-hoc prompt (no autosk task) — useful for one-offs:

```bash
autosk daemon submit --prompt "explore this repo" --thinking low
```

---

## Configuration

`autosk daemon serve` is CLI-flag-only in v0 (no TOML yet).

| Flag | Default | Effect |
|---|---|---|
| `--bind` | `127.0.0.1:7878` | TCP listen address. |
| `--token-file` | (empty) | If set, file contents are required as `Authorization: Bearer <token>` for everything except `/v1/healthz` and `/v1/version`. |
| `--workers` | `2` | Max concurrent pi processes. |
| `--cwd` | current dir | Default working directory passed to pi when a request omits `cwd`. |
| `--grace` | `10s` | Time SIGTERM has to bring pi down before SIGKILL. |
| `--idle-timeout` | `30m` | Max time between agent events on a single turn before failing the run. |
| `--pi-bin` | `pi` | pi binary to spawn (looked up on PATH unless absolute). |
| `--session-dir` | `<cwd>/.autosk/sessions` | Parent directory for per-job pi sessions. |

The runtime state of each job lives in `daemon_runs` inside the project's
`.autosk/db`. Surviving `daemon_runs` rows are still queryable when the
daemon is offline (via `autosk sql` or `autosk daemon status` once it is
back up). Rows whose `status='running'` at daemon startup are rewritten
to `failed` with `error='daemon_restart'`.

---

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| `POST`   | `/v1/jobs`                        | Submit a job (see §Request body). |
| `GET`    | `/v1/jobs`                        | List jobs. `?status=`, `?task_id=`, `?limit=`. |
| `GET`    | `/v1/jobs/{job_id}`               | Read one job. |
| `DELETE` | `/v1/jobs/{job_id}`               | Cancel (SIGTERM → grace → SIGKILL); idempotent on terminal rows. |
| `GET`    | `/v1/jobs/{job_id}/messages`      | `?limit=N` (≤500), `?full=true`. 410 Gone when session is missing. |
| `GET`    | `/v1/jobs/{job_id}/stream`        | SSE: `event: message`, `event: status`, `event: done`. Supports `Last-Event-ID`. |
| `GET`    | `/v1/healthz`                     | `{"ok":true,"workers":N,"queued":Q,"running":R}`. |
| `GET`    | `/v1/version`                     | autosk build info. |

### Submit body

```json
{
  "task_id":          "as-a1b2",
  "prompt":           "optional override; if absent, daemon renders from task",
  "model":            "sonnet:high",
  "thinking":         "high",
  "cwd":              "/abs/path",
  "auto_claim":       true,
  "max_corrections":  3,
  "extra_args":       ["--no-tools"]
}
```

Validation:

- At least one of `task_id`, `prompt` is required.
- `task_id` must exist in `tasks`.
- `cwd` must be absolute and a directory.
- `thinking` must be `off|minimal|low|medium|high|xhigh` or empty.
- `extra_args` rejects daemon-managed flags (`--model`, `--thinking`,
  `--mode`, `--session-dir`, `--session`, `--no-session`, `--print`,
  `-p`, `--api-key`, `--system-prompt`).

### Job response

See `internal/daemon/api/types.go::JobResponse`. The interesting fields:

| Field | Meaning |
|---|---|
| `status` | `queued|running|done|failed|cancelled`. |
| `closure_kind` | On success: `done|cancelled|decomposed`. |
| `corrections_used` / `max_corrections` | Kickback attempt counter. |
| `pi_session_id` / `session_path` | Captured from pi `get_state`; used by `messages`. |
| `pre_blocked_by` | Snapshot of `task.blocked_by` taken at run start. |
| `error` | Non-empty on terminal failure; `"agent_did_not_close_task"` when corrections were exhausted. |

---

## Closure verification

After every `agent_end` event the daemon classifies the autosk task:

| Verdict | Condition | Action |
|---|---|---|
| `done` | `tasks.status == done` | Run terminates as `done`. |
| `cancelled` | `tasks.status == cancelled` | Run terminates as `done` (the agent cancelled by design). |
| `decomposed` | Status still open AND at least one new blocker was added by the agent | Run terminates as `done`. |
| (invalid) | None of the above | Daemon sends a corrective user message; `corrections_used += 1`. After `max_corrections`, the run terminates as `failed` with `error="agent_did_not_close_task"`. |

The daemon **never** calls `autosk done`. The autosk extension running
inside pi is expected to do that.

---

## Security caveats

- **Loopback by default; no auth.** Anyone with a process on the host
  (and access to the bind port) can submit jobs. Use `--token-file` if
  this matters.
- **Token at rest.** A token file is exactly as secure as the filesystem
  permissions on `.autosk/daemon-token` (or wherever you point
  `--token-file`). Same trust boundary as `.autosk/db` itself.
- **Tool access is pi's.** The spawned pi inherits the parent
  environment, can shell, edit files, install dependencies, etc. Do not
  point `--cwd` at directories you would not give an interactive pi
  session access to.
- **Concurrent runs in the same cwd will race on files.** v0 has no
  worktree isolation. Either run one worker per cwd or pass distinct
  `cwd` values per submit (the request field is honoured).

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `submit_status 400 "task not found"` | The provided `task_id` does not exist in this `.autosk/db`. |
| Run sits in `running` forever | The agent never emits `agent_end`. The daemon will fail it after `--idle-timeout`. |
| Run fails with `agent_did_not_close_task` | The agent stopped without calling `autosk done|cancel` and without adding new blockers, `max_corrections` times in a row. Inspect transcript via `messages`. |
| Run fails with `daemon_restart` | The daemon was restarted while this run was active. v0 does not re-attach. Resubmit if needed. |
| 410 on `/v1/jobs/{id}/messages` | `session_path` is empty or the file was deleted. |
| Stream connection drops | Long polls may need `X-Accel-Buffering: no` (already sent). Use `Last-Event-ID` on reconnect to skip replay. |

---

## Open items (post-v0)

See plan §10. The big ones:

- Re-attach to surviving pi processes after a daemon restart.
- `POST /v1/jobs/{id}/retry`.
- Worktree-per-job isolation.
- TOML config file.
