# `autosk lazy` — interactive TUI

`autosk lazy` is a lazygit-style terminal dashboard for autosk. It
shows **tasks**, **jobs**, **workflows**, and **agents** in one
screen, lets you mutate any of them through hotkeys, and renders
each running job's transcript live in the Detail pane — with a
small input textarea below it that talks to the agent.

```bash
cd ~/your/project
autosk lazy
```

The TUI never replaces the CLI: every read and most writes are also
reachable through `autosk <verb>`. Use `lazy` when you want a denser,
faster front door.

---

## Launching

```bash
cd ~/your/project
autosk daemon serve &     # optional, but recommended for live job streams
autosk lazy
```

Without the daemon the dashboard still works — tasks, jobs,
workflows, and agents all render from `.autosk/db`, write hotkeys
still mutate the DB, and job transcripts render from each job's
`session.jsonl` archive. The pieces that **need** the daemon are the
live SSE stream into the Detail pane, the input textarea, and the
cancel-job verb. See [Daemon dependency](#daemon-dependency).

---

## Layout

![autosk lazy dashboard](lazy-mode.png)

Four left panels stacked vertically, a Detail pane on the right.
The focused side panel grows accordion-style so the highlighted row
is always visible. The bottom bar shows project root, daemon health,
worker stats, active filter / scope chips, and the help hint.

For a **terminal** job status (done / failed / cancelled) the layout is
identical but the input textarea is gone — the Detail pane reclaims
its space.

---

## What the Detail pane shows

The Detail pane always reflects the focused side panel:

- **Tasks** — task sheet: status header, description, recent jobs
  (≤5, with live indicator on the active one), recent comments
  (≤5, multi-line bodies render in full), recent step signals (≤3).
- **Jobs** — job header (id + status glyph + `workflow:step` +
  agent + timestamps + `attached` / `corrections` / `pid` /
  `session:` / `error:`), then one labelled box per transcript event,
  oldest first. For a running job a 6-row `input` textarea is
  pinned below the transcript.
- **Workflows** — workflow name, description, step graph (rendered
  with markdown), and an `isolation:` line carrying the current mode
  plus the count of non-terminal tasks currently using it.
- **Agents** — package name, version, install source (`builtin`,
  `installed`, or `db_only` when a referenced package isn't
  installed locally), and config summary.

The transcript merges two sources: the archive
(`session.jsonl` on disk, or the daemon's `/v1/jobs/{id}/messages`
when reachable) plus a live SSE tail for running jobs. Events are
deduplicated and ordered by timestamp. Re-visiting a job is
instant — every job's rendered boxes are cached in memory.

Each event box is labelled `<smart-datetime> <kind> [<name>]`.
Assistant events (`assistant_text`, `assistant_thinking`, and any
future `assistant_*` variant) render through the markdown renderer;
every other kind (`user_text`, `tool_call`, `tool_result`,
`session`, `model_change`, `compaction`, …) renders as plain text.

---

## Keymap

### Global

| Key | Action |
|---|---|
| `1` … `4` | Focus left panel by number. |
| `0` | Focus the Detail pane (`j/k/g/G/Ctrl-F/Ctrl-B/PgUp/PgDn` then scroll the detail content). |
| `Tab` | Cycle left panels. |
| `Enter` | Drill into the focused row (see panel-specific tables). |
| `Esc` | Pop one level (input → Jobs panel, popup → close, filter chip → drop). |
| `?` | Help cheatsheet overlay. |
| `:` | Command palette. Verbs from every panel: `task new`, `task edit`, `task done`, `task cancel`, `task reopen`, `task priority`, `task resume`, `task enroll`, `task block`, `task unblock`, `task comment`, `task metadata`, `workflow create`, `workflow delete`, `job cancel`, `scope clear`, `refresh`, `quit`. |
| `/` | Filter the focused panel — see [Filter syntax](#filter-syntax). |
| `*` | Clear all scope chips. |
| `R` | Force-refresh now (skip the periodic tick). |
| `Ctrl-R` | Hard refresh: drop the pooled DB connection, clear job-transcript cache, tear down live SSE. Use when external writes (CLI, daemon, another `lazy`) still don't appear after pressing `R`. |
| `@` | Toggle the command-log viewport. |
| `q` / `Ctrl-C` | Quit. |

### Tasks `[1]`

| Key | Action |
|---|---|
| `n` | New task — two-pane compose editor (summary + description). Empty summary cancels silently. |
| `c` | **Edit** the selected task — same two-pane editor pre-filled with the current title + description. |
| `d` | Mark **done** (confirms when status was `work`). |
| `x` | Cancel (confirms). |
| `o` | Reopen (`done` / `cancel` → `new`, preserves `workflow_id`). |
| `e` | Enroll into a workflow — prompts for the workflow name. |
| `r` | Resume (`human` → `work`); optionally to a named step. |
| `b` | Add a blocker (prompts for blocker id). |
| `u` | Remove a blocker (prompts for blocker id). |
| `m` | Add a comment — single-pane multi-line compose. `Enter` inserts a newline; `Ctrl-S` / `Alt-Enter` submits; `Esc` cancels. Empty submit is a silent cancel. |
| `p` | Set priority (`0` … `3` picker). |
| `M` | **Edit metadata** — single-pane editor pre-filled with the task's current `metadata` pretty-printed as JSON (`{}` when empty). On submit the body is parsed as a JSON object and replaces `tasks.metadata` wholesale. Invalid JSON or a non-object payload re-opens the popup with the error and the typed text intact. |
| `J` / `K` | Scroll the task-detail viewport. |

### Jobs `[2]`

| Key | Action |
|---|---|
| `Enter` | Running job → caret jumps into the `input` textarea below the Detail pane. Terminal job → logical focus moves to the Detail pane (`j` / `k` scroll the transcript). |
| `K` | Cancel job (confirms). |

Cursor moves on a running job open a live SSE subscription after a
short debounce so back-to-back `j` / `k` keystrokes don't churn the
stream.

### Workflows `[3]`

| Key | Action |
|---|---|
| `n` | Create from a JSON file — prompts for the path. |
| `D` | Delete (confirms). |
| `i` | Update **isolation** (`none` ↔ `worktree`). Opens a two-option menu with the current mode marked. Selecting the current value closes the popup silently. Selecting the other value chains into a confirm popup that enumerates affected non-terminal tasks (capped at 10 with a `… and N more` suffix); `y` invokes `UpdateWorkflowIsolation(…, force=true)`. Synthetic `single:*` rows drop a status-bar flash (`isolation is locked to 'none' on synthetic workflows`) and do NOT open the menu. Routes through the same `workflow.Store.UpdateIsolation` the CLI uses — see [docs/workflows.md § Updating isolation](workflows.md#updating-isolation) for the safety semantics. |

Isolated workflow rows render a muted `[wt]` marker after the
workflow name; synthetic rows never carry it. After a successful
`worktree → none` flip with leftover directories, the success
acknowledgement plus a leftover-cleanup hint share one info-level
flash (the leftover paths also land in the command log via `@`).

### Agents `[4]`

Read-only panel. Install or uninstall agents from the CLI:

```bash
autosk agent install   @your-org/developer
autosk agent uninstall @your-org/developer
```

### Detail pane (any entity)

Applies whenever the Detail pane has focus (`0`, or `Enter` on a
terminal Jobs row).

| Key | Action |
|---|---|
| `j` / `k` / `↑` / `↓` | Line scroll. |
| `Ctrl-F` / `Ctrl-B` / `PgDn` / `PgUp` | Page scroll. |
| `g` / `G` | Jump to top / bottom. |
| wheel | One line per tick. |

### Job input (running job only)

The 6-row `input` textarea pinned under the Detail pane.

| Key | Action |
|---|---|
| `Ctrl-D` | Send the textarea contents to the agent. The daemon decides whether it's a fresh prompt or a steer based on the agent's state. |
| `Ctrl-F` | Send the contents as a `follow_up` — queued and delivered at the start of the next agent turn. |
| `Ctrl-A` | Abort the in-flight agent turn. |
| `Esc` | Return focus to the Jobs panel; clear the buffer. |
| `Ctrl-B` / `PgUp` / `PgDn` | Page-scroll the Detail pane above without losing the input's text. |
| wheel | Scroll the Detail pane above. |

Dispatch targets the job the input was authored against — not the
current cursor. If a refresh shuffles the cursor onto a different
running job while you're typing, `Ctrl-D` / `Ctrl-F` / `Ctrl-A`
still route to the original job. The buffer also persists while the
cursor stays on the same running job; it clears on dispatch, on
`Esc`, or when you explicitly move the cursor to a different job.

When a running job transitions to a terminal status, the textarea
disappears on the next layout pass and focus reverts to the Jobs
panel.

---

## Filter syntax

`/` opens an incremental, case-insensitive filter on the focused
panel. The filter is rendered as a chip in the bottom bar; press
`Esc` to drop it.

Tasks accept structured facets followed by free text. The free text
is matched as a substring against id + title.

| Facet | Effect |
|---|---|
| `p:<n>` | Priority (`0` … `3`). |
| `status:<status>` | Task status. One of `new`, `work`, `human`, `done`, `cancel`. |
| `wf:<name>` | Workflow name. An unknown name returns zero rows so the filter never silently widens. |
| `agent:<name>` | Matches the task author **or** the current step's agent. |

Example:

```
/p:1 wf:feature-dev refactor
```

selects P1 tasks in `feature-dev` whose title or id contains
`refactor`.

The other panels (Jobs, Workflows, Agents) take a plain substring
query, matched against id + status + workflow + step name (or the
analogous fields per panel).

---

## Scope chips (cross-linking)

Moving the cursor in one panel updates the Detail pane and, for
some panels, narrows the others via a **scope chip** shown in the
bottom bar.

| Trigger | Effect |
|---|---|
| Cursor in **Tasks** | Jobs panel gets `scope: task=<id>` and filters to that task's runs. |
| Cursor in **Workflows** | Tasks **and** Jobs get `scope: wf=<name>` and filter to that workflow. |
| Cursor in **Jobs** | Detail pane updates only — no scope chip propagates back. |
| `Enter` in **Agents** | Opens a small picker (`by author` / `by current step` / `cancel`); the chosen relation becomes the chip, e.g. `scope: agent=@autosk/developer (author)`. |
| `*` (anywhere) | Clears every scope chip. |

Scope is additive: `wf=X` + `task=Y` narrows Jobs to runs of task
`Y` whose workflow is `X`. Conflicting chips just produce an empty
panel — nothing throws.

---

## Markdown rendering

The Detail pane renders user-supplied markdown as formatted ANSI:

- `Task.Description` (the `description` block on a task).
- Each entry in the `comments` block (multi-line bodies render in
  full; the full thread is rendered, oldest at the top and newest
  at the bottom — no display cap). The Detail pane scrolls and
  sticky-tails, so the newest comment stays on screen by default
  and older history is reachable by scrolling up.
- `Workflow.Description`.
- Assistant transcript events in the job Detail pane (any event
  whose `kind` begins with `assistant`).

Supported constructs are stock CommonMark: ATX headings,
`**bold**` / `*italic*`, ordered and unordered lists, blockquotes,
`inline code`, fenced code blocks, links, horizontal rules. Fenced
code blocks are syntax-highlighted; the language tag picks the
lexer, and unknown / empty tags fall back to plain monospace. Raw
UTF-8 emoji passes through; `:shortname:` shortcodes are **not**
expanded.

The compose popups (`n`, `c`, `m`, `M`) are raw editors — markdown
is rendered only when reading, never while typing. Wire formats
(CLI `--json`, daemon HTTP API, transcript JSON on disk) stay on
raw plain text; only the TUI display layer interprets markdown.

If the renderer fails on pathological input (deeply nested
blockquotes, very large bodies, …), the Detail pane falls back to
the raw text rather than blanking.

---

## Daemon dependency

`autosk lazy` adapts to the daemon's state — the status bar shows
which mode you're in:

| State | Status bar | Effect |
|---|---|---|
| **daemon ok** | `daemon=ok workers=N q=N r=N` | Jobs panel reads from the daemon's HTTP API (live `Streaming` / `AttachCount` columns). Live SSE attaches when the cursor settles on a running job. Cancel-job and the `input` textarea both work. |
| **daemon stale** | `daemon=stale` | Socket reachable but `/v1/healthz` returns an error. Treated the same as `down`. |
| **daemon down** | `daemon=down` | Jobs panel reads `daemon_runs` directly from `.autosk/db`. Live SSE is disabled. The Detail pane still renders the archive transcript from `session.jsonl`. The `input` textarea is hidden — there's no dispatch surface. Cancel-job returns an error. |

When the live datasource errors transiently (timeout, 5xx,
malformed body) the read falls back to the offline base for that
one call. If the fallback fired since the last tick, a `flaky+N`
chip appears in the bottom bar so a flaky daemon stays visible.

The dashboard polls every 2s by default (`--refresh` to change).
Cursor moves re-fetch the focused detail immediately rather than
waiting for the next tick.

### Cross-process freshness

`.autosk/db` is shared between every `autosk` process (CLI,
daemon, lazy). External writes appear on the next refresh tick.
Press `R` to force-refresh sooner, or `Ctrl-R` to drop the pooled
DB connection entirely and re-read — useful if a long-running
compactor (`autosk gc` / daemon GC) rewrote the file and you want
fresh fds immediately.

---

## Flags

| Flag | Default | Effect |
|---|---|---|
| `--sock <path>` | `$AUTOSK_SOCK` or `~/.autosk/daemon.sock` | Daemon UDS path. |
| `--refresh <dur>` | `2s` | Panel refresh cadence. |
| `--db <path>` | DB discovery rules | Override `.autosk/db` discovery. Equivalent to setting `$AUTOSK_DB`. |

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `daemon=down` but `autosk daemon list` works | Stale socket path. Pass `--sock` or set `$AUTOSK_SOCK`. |
| No `input` textarea on a job you know is running | Daemon is down or the live datasource just flipped the job to a terminal status. Start the daemon (`autosk daemon serve`) and the textarea reappears on the next tick. |
| Agents panel hotkey only flashes a message | Read-only by design — install / uninstall from the CLI. |
| Help screen lists `Ctrl-F` twice | Same chord, two view-scoped meanings: page-forward in the Detail pane, `follow_up` dispatch in the input textarea. The `?` overlay disambiguates by focus. |
| Detail pane shows `(loading…)` and stays there | Archive load is in flight; if it never resolves, check the daemon log or press `Ctrl-R` to drop the cache and retry. `(archive load failed: …)` means the underlying fetch errored — retry with `Ctrl-R`. |
| Signals / comments for a job are missing in the job Detail | They live on the parent **task** detail. Focus the Tasks panel (`1`) and move the cursor onto the parent task. |
| External CLI writes don't show up | Press `R` to force a refresh; if the data is still stale, `Ctrl-R` drops the DB connection and reopens. |
