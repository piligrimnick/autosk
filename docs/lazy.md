# `autosk lazy` — interactive TUI

`autosk lazy` is a lazygit-style terminal dashboard for the autosk
world: **tasks, jobs, workflows, and agents** in one process.
Selecting a job in the Jobs panel renders its transcript (offline
`session.jsonl` archive + live SSE tail) directly in the Detail
pane, one labelled box per event. For a running job a textarea
appears below Detail with `Ctrl-D send` / `Ctrl-F follow_up` /
`Ctrl-A abort` — same contract `autosk attach` had.

```bash
autosk lazy                    # dashboard
```

It doesn't replace any subcommand — every read and most writes are
also reachable through the CLI. `lazy` is a denser, faster front
door.

The design contract lives in
[`docs/plans/20260519-Lazy-Plan.md`](plans/20260519-Lazy-Plan.md);
the implementation plan and risk register live in
[`docs/plans/20260520-Lazy-Impl-Plan.md`](plans/20260520-Lazy-Impl-Plan.md).
The Detail-pane redesign that removed the fullscreen Inspector and
folded the transcript / input into the dashboard's Detail column
lives in
[`docs/plans/20260522-job-detail-redesign.md`](plans/20260522-job-detail-redesign.md).
This page is the user-facing recipe.

---

## Quickstart

```bash
cd ~/your/project
autosk daemon serve &                     # optional but recommended (live SSE needs it)
autosk lazy                               # opens the dashboard
```

Without the daemon the dashboard still works: Tasks / Jobs /
Workflows / Agents render from `.autosk/db`, write hotkeys still
mutate the DB, and the Detail-pane transcript renders against the
stored `session.jsonl` archive. The live SSE stream + live-input
textarea are the pieces that need `autosk daemon serve` — see
[§ Graceful degradation](#graceful-degradation).

---

## Replaces `autosk attach`

The standalone `autosk attach` command is **gone**. There is no
shim, no alias — running it prints cobra's unknown-command error.
To open a job's live mirror, run `autosk lazy`, focus the Jobs
panel (`2`), move the cursor onto the job. The transcript renders
inline in the Detail pane (right column); for a running job the
textarea pinned below Detail carries the same hotkey contract
`autosk attach` had — `Ctrl-D` send, `Ctrl-F` follow_up, `Ctrl-A`
abort, `Esc` returns focus to the Jobs panel.

The previous `--job <id>` deep-link flag is gone with the
fullscreen Inspector it used to launch; selecting the job in the
Jobs panel is the only path now.

---

## Layout

The dashboard always shows the four side panels on the left and the
Detail pane on the right. The Detail pane reflects the focused side
panel — a Tasks row renders task-detail (description + recent jobs
+ comments + signals); a Jobs row renders the job header +
per-event transcript boxes (described below); Workflows / Agents
render their own detail.

With a **Tasks** row focused the Detail pane is the task sheet:

```
┌─[1] Tasks ──────────────────────────────┬─[0] Detail ────────────────────────┐
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

With a **running Jobs** row focused, the Detail pane carries the
job header + transcript boxes and a small `input` textarea is
pinned below it. Terminal jobs are identical but without the
textarea — the Detail pane reclaims its space:

```
┌─[2] Jobs ───────────────────────────────┬─[0] Detail ────────────────────────┐
│ ◐ run-9ab1 stream feature-dev:dev   3m  │ run-9ab1 ◐ feature-dev:dev         │
│ ◯ run-77a2 done   feature-dev:rev  12m  │ agent=@autosk/developer            │
│ ✗ run-0c12 failed single:dev        1h  │ created 09:14:02  started 09:14:03 │
│   (filtered by task ask-a1b2c3)         │ attached 1  corrections 0/3  pid…  │
│                                         │ session: /tmp/autosk/run-9ab1/se…  │
│                                         │                                    │
│                                         │ ┌ 09:14:03 session ──────────────┐ │
│                                         │ │ cwd /Users/me/proj             │ │
│                                         │ └────────────────────────────────┘ │
│                                         │ ┌ 09:14:09 assistant_text ───────┐ │
│                                         │ │ Looking at the auth flow now…  │ │
│                                         │ └────────────────────────────────┘ │
│                                         │ ┌ 09:14:11 tool_call [Read] ─────┐ │
│                                         │ │ path=internal/auth/middlew…    │ │
│ (other side panels collapsed)           │ └────────────────────────────────┘ │
│                                         ├─ input (Ctrl-D send  Ctrl-F follow_up  Ctrl-A abort  Esc cancel) ─┐
│                                         │ plan: refactor the middleware       │
└─────────────────────────────────────────┴─────────────────────────────────────┘
 autosk · /Users/me/proj  daemon=ok  workers=2 q=0 r=1  scope: —  ?=help
```

The **job Detail pane** carries:

1. A structured header: `jobID` · status glyph · `workflow:step` ·
   `agent=<name>`, a meta row with `created` / `started` /
   `finished` smart-formatted timestamps, a third row with
   `attached` / `corrections` / `pid` (when set), a muted `session:`
   row, and an `error:` row in error style when `job.Error` is
   non-empty.
2. The transcript as one labelled box per event — archive +
   live-tail merged, oldest first. Box label:
   `<smart-datetime> <kind> [<name>]`. Assistant events
   (`assistant_text`, `assistant_thinking`, plus any future
   `assistant_*` variants) render through the markdown renderer;
   every other kind (`user_text`, `tool_call`, `tool_result`,
   `session`, `session_info`, `model_change`,
   `thinking_level_change`, `compaction`, `branch_summary`, `label`,
   `custom`, `other`) renders as plain text. Events with empty text
   render as just the label + frame.
3. A 6-row `input` textarea, only when the selected job is
   running. Its contents persist while the cursor stays on the same
   job (cleared on dispatch via `Ctrl-D` / `Ctrl-F`, on `Esc`, or
   when the cursor moves to a different job).

The focused side panel grows accordion-style so the selected row is
always visible. `@` toggles the command log; the bottom bar shows
project root, daemon health, worker stats, active scope chips, and a
`flaky+N` chip when the live datasource has fallen back to the DB
since the previous tick.

`Enter` on a **Jobs** row routes by status: for a **running** job
the caret moves into the `input` textarea (start typing immediately);
for a **terminal** job logical focus moves to the Detail pane so
`j` / `k` / `g` / `G` / `Ctrl-F` / `Ctrl-B` scroll the transcript.
`Esc` from the input textarea returns focus to the Jobs panel and
clears the buffer.

---

## Keymap

### Global

| Key | Action |
|---|---|
| `1` … `4` | Focus left panel by number. |
| `0` | Focus the Detail pane (enables `j/k/g/G/Ctrl-F/Ctrl-B/PgUp/PgDn` scroll on whichever entity's detail is showing). |
| `Tab` | Cycle left panels. |
| `Enter` | Drill into the focused row. On a Jobs row: running → focus the input textarea; terminal → focus the Detail pane. |
| `Esc` | Pop one level: input textarea → Jobs panel (also clears the buffer); popup → close; filter chip → drop. |
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
| `Enter` | Running job → caret jumps into the `input` textarea. Terminal job → logical focus moves to the Detail pane (`j`/`k` scroll the transcript). |
| `K` | Cancel job (`DELETE /v1/jobs/{id}`; confirms). |

Cursor moves on a running job open a Live SSE subscription after a
2 s debounce so back-to-back `j`/`k` keystrokes don't churn the
stream. The archive (`Messages(jobID, full=true)`) loads on every
cursor change without debounce — it's cheap and benefits from the
per-job cache. See [§ Job Detail pane](#job-detail-pane) for the
full contract.

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

### Detail pane (any entity)

Applies to the Detail viewport whenever it has focus — task-detail
and job-detail share the same scroll bindings.

| Key | Action |
|---|---|
| `j` / `k` / `↑` / `↓` | Line scroll. |
| `Ctrl-F` / `Ctrl-B` / `PgDn` / `PgUp` | Page scroll. |
| `g` / `G` | Jump to top / bottom. |
| wheel | One line per tick. |

### Job input (running job only)

The 6-row `input` textarea pinned under the Detail pane for a
running job. Same hotkey contract `autosk attach` had.

| Key | Action |
|---|---|
| `Ctrl-D` | Send the textarea contents to the agent (the daemon picks prompt vs steer). |
| `Ctrl-F` | Send the contents as a `follow_up` (queued for the next agent turn). |
| `Ctrl-A` | Abort the in-flight agent turn. |
| `Esc` | Return focus to the Jobs panel; clear the buffer. |
| `Ctrl-B` / `PgUp` / `PgDn` | Page-scroll the Detail pane **above** without losing the input's text. |
| wheel | Scroll the Detail pane above. |

Dispatch targets the job the input was authored against, not the
current cursor. After a refresh-driven reshuffle silently shifts the
cursor to a different running job, `Ctrl-D` / `Ctrl-F` / `Ctrl-A`
still route to the job whose draft the operator was typing. The
authored target is captured on the first keystroke and held until
the buffer is cleared.

`Ctrl-F` is overloaded by view focus: in the Detail pane it pages
forward through the transcript; in the `input` textarea it
dispatches `follow_up`. The `?` overlay disambiguates the two by
focus context.

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
- Assistant transcript events in the **job Detail pane** — any event
  whose `kind` starts with `"assistant"` (`assistant_text`,
  `assistant_thinking`, future `assistant_*` variants). Every other
  transcript kind (`user_text`, `tool_call`, `tool_result`,
  `session`, …) renders as plain text inside its box.

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

## Job Detail pane

Selecting a Jobs row renders the job in the right-hand Detail pane:
structured header (jobID + status glyph + `workflow:step` + agent +
timestamps + attached/corrections/pid + session/error) on top,
followed by one labelled box per transcript event. There are no
tabs, no fullscreen modal — the Detail pane is the only surface.

### Transcript merge (archive + live)

The transcript section combines two sources:

- **Archive** — `Messages(jobID, full=true)` against the daemon
  when reachable; the on-disk `session.jsonl` otherwise. Loaded on
  every cursor change, but coalesced per-job through a freshness
  check so back-to-back cursor moves on the same job don't pile on
  requests.
- **Live tail** — SSE subscription opened after a 2 s debounce
  whenever the cursor settles on a running job. Same throttle the
  previous inspector used (lazygit's `pkg/tasks/tasks.go` pattern,
  ~30 ms coalesce on render). A bounded ring buffer keeps the last
  ~2000 events; on overflow a muted
  `(transcript truncated; older events dropped to cap memory)`
  line precedes the boxes. Re-fetching the archive (e.g. on a
  running→terminal transition) drops the truncation flag once the
  full set arrives.

The two streams are merged by timestamp — archive replaces the
prefix, and any live event with `TS` strictly after the archive's
last event is preserved. Re-selecting a previously-visited job
renders instantly from a per-job in-memory cache (capped at 32
entries; least-recently-touched victim on overflow; terminal entries
refetch after 30 s).

### Event boxes

Each event renders as one `drawLabeledBox`:

- **Label.** `<smart-datetime> <kind> [<name>]`. `<kind>` is
  bold-styled in the header hue; `<name>` (the tool name on
  `tool_call` / `tool_result`, optional on other kinds) is appended
  in the accent hue.
- **Body.** Assistant events (anything whose `kind` starts with
  `"assistant"` — currently `assistant_text` and
  `assistant_thinking`, plus any future variants) render through the
  markdown renderer used by the Tasks-detail pane. Every other kind
  (`user_text`, `tool_call`, `tool_result`, `session`,
  `session_info`, `model_change`, `thinking_level_change`,
  `compaction`, `branch_summary`, `label`, `custom`,
  `custom_message`, `other`) renders as plain text. An event with
  empty `Text` renders as just the label + frame.
- **Width.** Each box is rebuilt when the pane width changes (window
  resize). Within a single width the rendered string is cached on
  the per-job entry so spinner ticks and refresh-driven redraws
  reuse the existing strings instead of re-laying out the markdown.

There are no signals / comments-since-run sections in the job
Detail pane — those still live in the task-detail pane (focus the
Tasks panel and move the cursor to the parent task).

### Sticky tail

When the operator's viewport is at the bottom of the Detail pane,
new live events scroll the viewport so the new event is visible.
When they've scrolled up, the viewport origin stays put across
live-append + refresh redraws. A first-frame default (empty buffer
→ newly populated) starts at the bottom.

### Live input semantics

The `input` textarea below the Detail pane (running jobs only):

- The buffer text persists while the cursor stays on the same
  running job. Cleared by `Ctrl-D` / `Ctrl-F` dispatch, by `Esc`,
  and when the operator moves the cursor (explicit `j`/`k`) to a
  different job.
- Refresh-driven cursor shifts (the datasource inserts a brand-new
  job at index 0, pushing the previously-cursored row down) do **not**
  clear the draft — they're not operator-authored so the draft
  survives, and dispatch still targets the originally-authored job.
- A running→terminal transition removes the textarea on the next
  layout pass; focus reverts to the Jobs panel so the operator
  doesn't end up on a deleted view.

### Hard reset (`Ctrl-R`)

In addition to dropping the pooled doltlite connection (see
[§ Cross-process freshness](#cross-process-freshness)), `Ctrl-R`
clears the entire job-transcript cache and tears down the live SSE
subscription. The next selection re-hydrates from scratch.

---

## Graceful degradation

`autosk lazy` is useful without the daemon:

| State | Status bar | Effect |
|---|---|---|
| **daemon ok** | `daemon=ok workers=N q=N r=N` | Jobs panel from `GET /v1/jobs` (includes `Streaming` / `AttachCount`); live SSE attaches when the cursor settles on a running job; cancel-job works. |
| **daemon stale** | `daemon=stale` | UDS dials but `/v1/healthz` 5xx. Same surface as `down`. |
| **daemon down** | `daemon=down` | Jobs panel reads `daemon_runs` from `.autosk/db`. Live SSE is disabled; the Detail pane still renders the archive transcript. The `input` textarea is hidden when the daemon is down (no dispatch surface). Cancel-job returns `ErrDaemonRequired`. |

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
| `--sock <path>` | `$AUTOSK_SOCK` or `~/.autosk/daemon.sock` | Daemon UDS path. |
| `--refresh <dur>` | `2s` | Panel refresh cadence. |

The previous `--job <id>` deep-link flag was removed together with
the fullscreen Inspector it used to launch. To open a job's
transcript from the command line, run `autosk lazy`, press `2` to
focus the Jobs panel, and move the cursor onto the job.

The global `--db <path>` and `AUTOSK_DB` env var work the same way
they do for every other write-capable verb (override DB discovery).

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `daemon=down` but `autosk daemon list` works | Stale socket path. Pass `--sock` or set `AUTOSK_SOCK`. |
| No `input` textarea on a job you know is running | Daemon down or the live datasource flipped the job to a terminal status mid-frame. `autosk daemon serve` restores SSE; the textarea reappears on the next layout pass. |
| `i` on Agents only flashes a message | By design — `lazy` can't fork npm installs from inside the TUI. Quit and run `autosk agent install <pkg>`. |
| Help screen lists `Ctrl-F` twice | Same chord, two view-scoped meanings — page-forward on the Detail pane, `follow_up` dispatch in the `input` textarea. The `?` overlay labels each by focus. |
| Detail pane shows `(loading...)` and stays there | Archive load is in flight; if it doesn't resolve, check the daemon log or `Ctrl-R` to drop the cache and retry. `(archive load failed: ...)` in error style means the underlying fetch errored — retry with `Ctrl-R`. |
| Signals / comments-since-run not visible for a job | They live on the **task** detail now, not the job detail. Focus the Tasks panel (`1`), move the cursor onto the parent task. |

---

## Out of scope (v1)

Same as the design plan §10: no `--all-projects`, no mouse selection
(wheel scroll only), no custom keymaps / config file / theming, no
demo-mode / GIF capture, no reconnect/backoff in the SSE client, no
half/full/normal screen modes. The flat Dashboard layout is enough.

Not pursued in the Detail-pane redesign either:

- Filtering transcript content by event kind (e.g. "hide tool_call").
- An in-transcript search prompt.
- A dedicated Detail pane for Workflows / Agents — only the Job
  Detail surface changed in this redesign.
- Persisting the `input` textarea contents to disk across `q`.

See the design contract
[`docs/plans/20260519-Lazy-Plan.md`](plans/20260519-Lazy-Plan.md) §10/§12
and the impl plans
[`docs/plans/20260520-Lazy-Impl-Plan.md`](plans/20260520-Lazy-Impl-Plan.md) §11 +
[`docs/plans/20260522-job-detail-redesign.md`](plans/20260522-job-detail-redesign.md)
for the canonical lists.
