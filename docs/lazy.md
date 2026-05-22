# `autosk lazy` — interactive TUI

`autosk lazy` is a lazygit-style terminal dashboard for the autosk
world: **tasks, jobs, workflows, and agents** in one process, with a
fullscreen job inspector that fuses a live agent-session mirror
(SSE) with an offline `session.jsonl` archive view.

```bash
autosk lazy                    # dashboard
autosk lazy --job run-9ab1     # deep-link straight into the inspector
```

It doesn't replace any subcommand — every read and most writes are
also reachable through the CLI. `lazy` is a denser, faster front
door.

The design contract lives in
[`docs/plans/20260519-Lazy-Plan.md`](plans/20260519-Lazy-Plan.md);
the implementation plan and risk register live in
[`docs/plans/20260520-Lazy-Impl-Plan.md`](plans/20260520-Lazy-Impl-Plan.md).
This page is the user-facing recipe.

---

## Quickstart

```bash
cd ~/your/project
autosk daemon serve &                     # optional but recommended (Live tab needs it)
autosk lazy                               # opens the dashboard
```

Without the daemon the dashboard still works: Tasks / Jobs /
Workflows / Agents render from `.autosk/db`, write hotkeys still
mutate the DB, and the inspector's Archive / Meta / Signals tabs
work against the stored session. The Live tab is the one piece that
needs `autosk daemon serve` — see [§ Graceful degradation](#graceful-degradation).

---

## Replaces `autosk attach`

The standalone `autosk attach` command is **gone** in this release.
There is no shim, no alias — running it prints cobra's
unknown-command error. To open a job's live mirror from the command
line use:

```bash
autosk lazy --job <job-id>
```

`Esc` (or `Ctrl-O`) inside the inspector returns to the dashboard
with the same job still selected. The Live tab carries the same
hotkey contract `autosk attach` had: `Ctrl-D` send, `Ctrl-F`
follow_up, `Ctrl-A` abort.

---

## Layout

```
┌─[1] Tasks ──────────────────────────────┬─[0] Detail / Inspector ────────────┐
│ ask-a1b2c3 ●P1 work   ▶dev   Refactor   │ task ask-a1b2c3  work              │
│ ask-c3d4e5  P1 work   ▶rev   Add CLI    │ wf=feature-dev step=dev (developer)│
│ ask-e5f617  P2 done          Bump ver   │ author: @autosk/developer          │
│ ask-778899 ✋P0 human  ▶qa    Tune w…    │ priority: 1   blocked: false       │
├─[2] Jobs ───────────────────────────────┤ ─ description ───────────────────  │
│ ◐ run-9ab1 stream  feature-dev:dev 3m   │ Refactor the auth flow so that …  │
│ ◯ run-77a2 done    feature-dev:rev 12m  │                                    │
│ ✗ run-0c12 failed  single:dev      1h   │ ─ recent jobs (3) ──────────────── │
│   (filtered by task ask-a1b2c3)         │   run-9ab1 running *live*(1)  3m   │
├─[3] Workflows ──────────────────────────┤   run-77a2 done             12m   │
│ feature-dev      3 steps                │   run-0c12 failed idle_to    1h   │
│ qa-loop          4 steps                │                                    │
│ single:@autosk/developer  1 step        │ ─ comments (≤5) ───────────────── │
├─[4] Agents ─────────────────────────────┤   2026-05-19 16:02 human: looks…   │
│ ● human                       builtin   │   2026-05-19 16:31 dev: ok will…  │
│ ● @autosk/developer  v0.3.1  installed  │                                    │
│ ● @autosk/reviewer   v0.2.4  installed  │ ─ recent signals (≤3) ─────────── │
│ ! @autosk/qa          —      db_only    │   2026-05-19T16:31 dev → review   │
└─────────────────────────────────────────┴────────────────────────────────────┘
 autosk · /Users/me/proj  daemon=ok  workers=2 q=0 r=1  scope: task=ask-a1b2c3  ?=help
```

The focused side panel grows accordion-style so the selected row is
always visible. `@` toggles the command log; the bottom bar shows
project root, daemon health, worker stats, active scope chips, and a
`flaky+N` chip when the live datasource has fallen back to the DB
since the previous tick.

`Enter` on a **Jobs** row hides the dashboard and opens a near-
fullscreen inspector. `Esc` (or `Ctrl-O`) restores the dashboard
with the same job still selected.

---

## Keymap

### Global

| Key | Action |
|---|---|
| `1` … `4` | Focus left panel by number. |
| `0` | Focus the right detail (enables `j/k` scroll on Tasks detail). |
| `Tab` | Cycle left panels. |
| `Enter` | Drill into the focused row. On Jobs → fullscreen inspector. |
| `Esc` | Pop one level: inspector → dashboard; popup → close; filter chip → drop. |
| `?` | Help cheatsheet. |
| `:` | Command palette. Verbs from every panel: `task new`, `task edit`, `task done`, `task cancel`, `task reopen`, `task priority`, `task resume`, `task enroll`, `task block`, `task unblock`, `task comment`, `task metadata`, `workflow create`, `workflow delete`, `job cancel`, `scope clear`, `refresh`, `quit`. |
| `/` | Filter the focused panel. See [§ Filter language](#filter-language). |
| `*` | Clear all scope chips. |
| `R` | Force-refresh (ignore the 2s tick). |
| `Ctrl-R` | Hard refresh: drop the pooled doltlite connection and re-open the DB. Use when external writes (`autosk` CLI, daemon, another lazy) still don't appear after pressing `R`. See [§ Cross-process freshness](#cross-process-freshness). |
| `@` | Toggle command-log visibility. |
| `q` / `Ctrl-C` | Quit. |

### Tasks `[1]`

| Key | Action |
|---|---|
| `n` | New task — opens the two-pane compose editor (summary + description). Empty summary cancels silently. |
| `c` | **Edit** the selected task — opens the same two-pane compose editor, pre-filled with the current `title` and `description`. Empty title after edit → flash `title required` and the popup stays open with the typed text intact. |
| `d` | Mark **done** (confirms when status was `work`). |
| `x` | Cancel (confirms). |
| `o` | Reopen (`done`/`cancel` → `new`, preserves `workflow_id`). |
| `e` | Enroll into a workflow — prompts for the workflow name. |
| `r` | Resume (`human` → `work`); optionally to a named step. |
| `b` | Add a blocker (prompts for blocker id). |
| `u` | Remove a blocker (prompts for blocker id). |
| `m` | Add a comment — single-pane multi-line compose. `Enter` inserts `\n`, `Ctrl-S` / `Alt-Enter` submit, `Esc` cancels. Empty submit is a silent cancel. |
| `p` | Set priority (`0..3` picker). |
| `M` | **Edit metadata** — single-pane compose pre-filled with the task's current `metadata` pretty-printed as JSON (`{}` when empty). On submit the body is `json.Unmarshal`-ed into a `map[string]any` and replaces `tasks.metadata` wholesale; invalid JSON or a non-object payload (array, string, number, `null`) re-opens the popup with `invalid JSON: …` and the typed text intact. |
| `J` / `K` | Scroll the Tasks detail viewport. |

There is **no `c claim`** binding — `c` is bound to **change/edit**
as of this release. The v0.2 schema has no claim verb anyway; tasks
self-advance via workflow steps. Use `e` to enroll a `new` task
into a workflow.

### Jobs `[2]`

| Key | Action |
|---|---|
| `Enter` / `a` | Open inspector at the default tab (Live for non-terminal runs, Archive for terminal). `a` always picks Live. |
| `s` | Open inspector at the Archive tab. |
| `i` | Open inspector at the Meta tab. |
| `K` | Cancel job (`DELETE /v1/jobs/{id}`; confirms). |

### Workflows `[3]`

| Key | Action |
|---|---|
| `n` | Create from a JSON file — prompts for the path. |
| `D` | Delete (confirms). |

### Agents `[4]`

Read-only panel — no hotkeys. `autosk lazy` cannot fork npm
installs from inside the TUI; install / uninstall from the CLI
with `autosk agent install <pkg>` and `autosk agent uninstall
<pkg>`.

### Inspector

| Key | Action |
|---|---|
| `[` / `]` | Cycle tabs. |
| `1..4` | Jump to a tab (1 Live · 2 Archive · 3 Meta · 4 Signals). |
| `Esc` / `Ctrl-O` | Back to dashboard. |
| `j` / `k` / `↑` / `↓` | Scroll body (when the input view is **not** focused). |
| `Ctrl-F` / `Ctrl-B` / `PgUp` / `PgDn` | Page-forward / page-back the body. |
| `g` / `G` | Top / bottom of the body. |

#### Live tab (textarea focus)

| Key | Action |
|---|---|
| `Ctrl-D` | Send the textarea contents as a prompt/steer. |
| `Ctrl-F` | Send the contents as a `follow_up` (queued for the next agent turn). |
| `Ctrl-A` | Abort the in-flight agent turn. |
| `Ctrl-B` / `PgUp` / `PgDn` | Scroll back through the transcript above. |

`Ctrl-F` is overloaded by view focus: in the body it pages forward
through the transcript; in the Live input textarea it dispatches
`follow_up`. The `?` overlay disambiguates the two by focus context.

---

## Filter language

`/` opens an incremental, case-insensitive filter on the focused
panel. Tasks accept `key:value` facets; free text after the facets
is matched as a substring against id + title.

| Facet | Effect |
|---|---|
| `p:<n>` | Priority. |
| `status:<status>` | Task status (`new`, `work`, `human`, `done`, `cancel`). Legacy spellings (`in_workflow`, `human_feedback`, `cancelled`) are rejected. |
| `wf:<name>` | Workflow name (resolved to wf-id). An unknown name returns zero rows so the chip never silently widens. |
| `agent:<name>` | Matches author **or** current-step agent (broadest sense). |

Example: `/p:1 wf:feature-dev refactor` selects P1 tasks in
`feature-dev` whose title or id contains `refactor`. The remaining
panels (Jobs / Workflows / Agents) take plain substring queries; the
search is applied to id + status + workflow + step name.

---

## Scope chips (cross-linking)

Moving the cursor in one panel re-renders the right detail and, for
some panels, narrows the others via a **scope chip**. The active
chips are listed in the bottom bar.

| Trigger | Effect |
|---|---|
| Move cursor in **Tasks** | Right detail re-renders. Jobs panel gets `scope: task=<id>` and filters to that task's runs. |
| Move cursor in **Workflows** | Right detail re-renders. Tasks **and** Jobs get `scope: wf=<name>` and filter to that workflow. |
| Move cursor in **Jobs** | Right detail re-renders. No chip propagates back. |
| `Enter` in **Agents** | Opens an opt-in popup (`by author` / `by current step` / `cancel`) — the author / current_step.agent relation is ambiguous so the operator picks. The chip shows the chosen relation, e.g. `scope: agent=dev (author)`. |
| `*` (anywhere) | Clears every scope chip. |

Scope is **additive**: `wf=X` + `task=Y` filters to runs of task `Y`
where the task's workflow is `X`. Conflicts just produce an empty
panel.

---

## Markdown in the Detail pane

The right-hand Detail pane renders user-supplied markdown as
formatted ANSI rather than raw text. This applies to:

- `Task.Description` (the `─ description ─` block on a Tasks row).
- Every entry in the `─ comments ─` block of a task. Bodies are no
  longer clipped to one line — multi-line comments render in full,
  with the "last 5" cap preserved.
- `Workflow.Description` (the right pane when a Workflows row is
  focused).

Supported constructs are stock CommonMark: ATX headings (`#`..`######`),
`**bold**` / `*italic*`, ordered and unordered lists, blockquotes,
`inline code`, fenced code blocks, links, horizontal rules. Fenced
code blocks are syntax-highlighted via [chroma](https://github.com/alecthomas/chroma)
(bundled with [glamour](https://github.com/charmbracelet/glamour),
the renderer); the language tag picks the lexer, and unknown / empty
tags fall back to plain monospace. Raw UTF-8 emoji (🚀) renders
through the normal text path; `:shortname:` shortcodes are
intentionally **not** expanded.

Element colours come from the active palette (`theme.Active()`).
Swapping the palette at runtime (`theme.SetActive` +
`tui.RebuildStyles`) updates the markdown render in lockstep with
the rest of the TUI.

The **compose popups** (two-pane `n`/`c` editor for title +
description, single-pane `m` comment editor, single-pane `M`
metadata-JSON editor) all stay **raw editors** — markdown is
rendered only when reading, never while typing.

Wire formats are untouched: `autosk` CLI `--json` output, the daemon
HTTP API, the `RunContextSeed` handed to agents, and the prompt
rendering used by `comments.RenderForPrompt` all stay on raw UTF-8
plain text. Only the TUI display layer interprets markdown.

Fail-open behaviour: if glamour cannot build a renderer, errors
mid-render, panics, or is handed pathological input (deeply nested
blockquotes, >64 KiB body), the Detail pane falls back to the raw
markdown text rather than crashing or blanking the pane.

---

## Inspector tabs

`Enter` on a job opens the inspector. The default tab depends on the
job's terminal status: **Live** for `queued`/`running`, **Archive**
for `done`/`failed`/`cancel`. Use `1..4` or `[ ` / `]` to switch.

### Live

SSE replay + tail of the in-flight session. Daemon required. Carries
the same hotkey contract `autosk attach` had — `Ctrl-D` send,
`Ctrl-F` follow_up, `Ctrl-A` abort, plus `Ctrl-B` / `PgUp` / `PgDn`
to scroll back through earlier transcript.

The SSE pump throttles render updates with a 30 ms coalesce window
(lazygit's `pkg/tasks/tasks.go` pattern) and keeps the last 2000
events in a ring buffer. When the buffer overflows the Live body
shows a one-line `(truncated)` indicator at the top.

When `lazy` runs without the daemon, the Live tab is disabled and
flashes a hint to `Archive` instead.

### Archive

Read-only renderer of the job's `session.jsonl` on disk. Pulled via
`GET /v1/jobs/{id}/messages?full=true` when the daemon is up; read
directly when it's down. No SSE, no input. `g`/`G` jump to
top/bottom, `Ctrl-F`/`Ctrl-B` page, `j`/`k` line scroll.

### Meta

A static key/value sheet rendered from `api.JobResponse`: job_id,
task, workflow:step, agent, status, streaming, attached count,
corrections, session path, PID, creation/start/finish timestamps,
duration, error, exit code.

### Signals

Two stacked sub-regions:

1. **Step signals for this run** (`step_signals` rows whose
   `run_id` matches the inspected job). The Signals tab is keyed by
   job-id — rows from earlier kickback runs of the same task do
   *not* bleed in.
2. **Comments observed during this run** — task comments filtered
   to `created_at >= job.StartedAt`. When the run hasn't started yet
   (queued / dispatched) the cutoff is dropped and all comments
   render.

Timestamps render as the full local `YYYY-MM-DD HH:MM:SS` (via
`internal/timeformat.FormatDateTime`) so a kickback chain spanning
midnight, or a run from yesterday opened today, is unambiguous. The
dashboard task-detail timeline (which usually shows today's events)
uses the smart variant — bare `HH:MM:SS` when the event is today in
the operator's TZ, full datetime otherwise.

---

## Graceful degradation

`autosk lazy` is useful without the daemon:

| State | Status bar | Effect |
|---|---|---|
| **daemon ok** | `daemon=ok workers=N q=N r=N` | Jobs panel from `GET /v1/jobs` (includes `Streaming` / `AttachCount`); Live tab opens SSE; cancel-job works. |
| **daemon stale** | `daemon=stale` | UDS dials but `/v1/healthz` 5xx. Same surface as `down`. |
| **daemon down** | `daemon=down` | Jobs panel reads `daemon_runs` from `.autosk/db`. Live tab disabled with a flash hint. Archive still works. Cancel-job returns `ErrDaemonRequired`. |

When the live datasource read errors transiently (timeout, 5xx,
malformed body) the verb falls back to the offline base for that
one call and increments a counter. The status bar shows a
`flaky+N` chip when the counter advanced between refresh ticks so a
daemon that's technically reachable but flaky is visible at a
glance.

The dashboard polls every 2s by default (`--refresh` to change).
Cursor moves re-fetch the focused detail immediately rather than
waiting for the tick.

### Cross-process freshness

`.autosk/db` is shared between every `autosk` process (CLI, daemon,
lazy). Doltlite is single-writer at the file-lock level, but there's
one gotcha for long-lived processes: `SELECT dolt_gc()` (the daemon's
compactor every ~30m, or `autosk gc` on demand) rewrites the database
via write-to-sidecar + atomic rename, so the on-disk inode changes.
A process that opened the file before gc keeps its fd on the orphan
inode and would serve the pre-gc snapshot forever — worse, an unwary
writer on that conn would silently route its INSERTs into the orphan
where they vanish when the conn closes.

Defence lives in the doltlite driver wrapper
(`internal/store/doltlite/driver.go`): every pool checkout validates
the path's current inode against the inode the conn was opened
against. When they disagree the conn is reported as bad, Go retires
it, and the next acquisition opens a fresh one against the live
file. Belt-and-suspenders: lazy also ties `SetConnMaxLifetime` to
`--refresh` so even an unchanged conn rotates every few seconds.

`Ctrl-R` is the explicit escape hatch: it drops the pooled conn
immediately (one fresh `Ping`) and triggers a refresh. Useful when
you want "give me whatever is on disk RIGHT NOW" instead of waiting
for the next acquisition.

Known residual race: a gc that finishes mid-statement (or mid-tx)
can still lose the in-flight write because the validator only fires
between statements, not inside one. With ~ms statements and a ~30m
gc cadence the per-write probability is ~10⁻⁷. Closing this would
require a cross-process advisory lock between the compactor and
writers; tracked as future work.

---

## Flags

| Flag | Default | Effect |
|---|---|---|
| `--job <id>` | unset | Deep-link: open the inspector directly on this job; `Esc` returns to the dashboard. |
| `--sock <path>` | `$AUTOSK_SOCK` or `~/.autosk/daemon.sock` | Daemon UDS path. |
| `--refresh <dur>` | `2s` | Panel refresh cadence. |

The global `--db <path>` and `AUTOSK_DB` env var work the same way
they do for every other write-capable verb (override DB discovery).

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `daemon=down` but `autosk daemon list` works | Stale socket path. Pass `--sock` or set `AUTOSK_SOCK`. |
| Live tab flashes `daemon required (try Archive)` | Daemon isn't running or doesn't have the SSE / attach hubs wired. `autosk daemon serve`. |
| Agents panel has no `i` / `u` hotkeys | By design — `lazy` can't fork npm installs from inside the TUI. Quit and run `autosk agent install <pkg>` / `autosk agent uninstall <pkg>`. |
| Help screen lists `Ctrl-F` twice | Same chord, two view-scoped meanings — page-forward on the inspector body, `follow_up` dispatch in the Live textarea. The `?` overlay labels each by focus. |
| Inspector tab shows `(no signals)` for a run you know emitted one | Confirm you're on the right run — the Signals tab is jobID-scoped, not taskID-scoped. Earlier kickback runs of the same task render in their own inspector. |

---

## Out of scope (v1)

Same as the design plan §10: no `--all-projects`, no mouse, no
custom keymaps / config file / theming, no demo-mode / GIF capture,
no reconnect/backoff in the SSE client, no half/full/normal screen
modes. The flat Dashboard ↔ Inspector switch is enough.

See the design contract
[`docs/plans/20260519-Lazy-Plan.md`](plans/20260519-Lazy-Plan.md) §10/§12
and the impl plan
[`docs/plans/20260520-Lazy-Impl-Plan.md`](plans/20260520-Lazy-Impl-Plan.md) §11
for the canonical list.
