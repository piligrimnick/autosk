# `autosk lazy` — interactive TUI

`autosk lazy` is a lazygit-style terminal dashboard for autosk. It
shows **tasks**, **sessions**, **workflows**, and **agents** in one
screen, lets you mutate tasks through hotkeys, and renders each
running **session's** transcript live in the Detail pane — with a
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
autosk lazy
```

> **Architecture note.** `autosk lazy` is a pure JSON-RPC client of the
> **`autoskd`** daemon, which it **auto-spawns** on first use (there is
> no Go `daemon serve` verb). Reads, every write hotkey, live session
> streaming into the Detail pane (`session.subscribe`), the input
> textarea (`ctrl+d` / `ctrl+f` / `ctrl+a`), and the abort-session verb
> all run over RPC against `autoskd`; lazy never opens the project
> store directly and has no offline or degraded mode. Every request
> carries only the `{cwd}` project selector. See
> [`docs/daemon.md`](daemon.md) for the daemon itself.

---

## Layout

![autosk lazy dashboard](lazy-mode.png)

Four left panels stacked vertically, a Detail pane on the right.
The focused side panel grows accordion-style so the highlighted row
is always visible. The bottom of the screen carries two pinned
single-row strips:

- The **status bar** shows project root, daemon health, worker
  stats, and the active filter / scope chips. Top-level blocks are
  separated with ` | `; tokens inside a block stay
  single-space-separated. No `?=help` element — that hint now
  lives on the options strip below.
- The **options strip** (very bottom row) is a context-aware list
  of `<key>: <action>` entries for the focused panel, joined with
  ` | `. The focused panel's high-traffic verbs come first, then
  the global staples (`?` help, `/` filter, `:` palette, `*`
  clear scope, `q` quit). The strip truncates with `…` when it
  overflows. Use it as a quick reminder of what the current panel
  can do; press `?` for the full sectioned cheatsheet.

For a **terminal** session status (done / failed / aborted) the layout
is identical but the input textarea is gone — the Detail pane reclaims
its space.

---

## What the Detail pane shows

The Detail pane always reflects the focused side panel:

- **Tasks** — task sheet: header line (`<id> <status>
  <workflow:step>`), an optional `blocked by:` row, a stats row
  (created + comment count), a `Title` box, a `Description` box
  (rendered as markdown), a `Metadata` box, and one box per comment
  (multi-line bodies render in full, oldest at the top). The
  `Metadata` box pretty-prints the task's free-form metadata bag;
  when it carries the engine's reserved `step_visits` counter it
  leads with a compact `visits: dev×2, review×1` summary line, and an
  empty bag shows `(no metadata)`. There is no priority field in v2.
- **Sessions** — session header (id + status glyph + `workflow:step`
  + `agent=` + started / ended timestamps + parent `task:` +
  `error:` when present), then one labelled box per transcript
  line, oldest first. For a running session a 6-row `input`
  textarea is pinned below the transcript.
- **Workflows** — header line `<name> [wt]? first step: <step>`
  (the `[wt]` chip appears iff the workflow is non-synthetic and its
  isolation is `worktree`), the description rendered as markdown,
  then a `Steps (N)` labelled box with one row per step in
  `<step> agent=<agent> next=<targets|(none)>` form. Columns are
  aligned: the `agent=` chip starts at the same column on every
  row, and the `next=` chip likewise. Sibling-step targets render
  in the step hue; lifecycle terminals (`done` / `cancel` /
  `human`) take their task-status hue. The pane is **read-only** —
  v2 workflows are code registered by extensions, not editable DB
  rows.
- **Agents** — the agent name plus a note that it is registered by
  an extension (v2 agents are code, not installed packages).

The transcript merges two sources: the archive (the daemon's
`session.transcript` RPC, served from the on-disk pi-format
transcript) plus a live `session.subscribe` tail for running
sessions. Events are deduplicated and ordered by line number.
Re-visiting a session is instant — every session's rendered boxes
are cached in memory.

Each transcript line is one of three pi-format kinds:

- **`session`** — the header line for the run, labelled `Session:
  <agent> on <step>` with a `Started: <datetime>` body.
- **`message`** — a user / assistant / tool-result turn, labelled
  `<smart-datetime> <role> [<toolName>]`. Assistant text renders
  through the markdown renderer; user / tool-result text renders as
  plain text. A tool-only assistant turn surfaces its tool-call
  names (`→ Read`, `→ Bash`, …) so the box is never empty.
- **`custom`** — an `autosk:*` lifecycle entry, labelled with the
  custom type (the `autosk:` prefix stripped) and the raw JSON body.

All timestamps render in the operator's local timezone via
`internal/timeformat`; the wire format stays RFC3339 UTC.

---

## Keymap

### Global

| Key | Action |
|---|---|
| `1` … `4` | Focus left panel by number (Tasks / Sessions / Workflows / Agents). |
| `0` | Focus the Detail pane (`j/k/g/G/ctrl+f/ctrl+b/pgup/pgdn` then scroll the detail content). |
| `tab` | Cycle left panels. |
| `enter` | Drill into the focused row (see panel-specific tables). |
| `esc` | Pop one level (input → Sessions panel, popup → close, filter chip → drop). |
| `?` | Help cheatsheet overlay — sectioned `--- Local --- / --- Global --- / --- Navigation ---` view of the focused panel's bindings. Type to filter (case-insensitive substring against key + description), `j` / `k` / arrows / wheel move the cursor, `enter` closes the popup AND invokes the highlighted binding, `backspace` pops a filter rune, `esc` clears the filter or (if already empty) closes the popup. Only the focused panel's local bindings plus the global bindings are listed — bindings of other panels are hidden. |
| `ctrl+w` | What's new — open the [changelog modal](#changelog-modal) with the full embedded `CHANGELOG.md`. Does NOT mutate `~/.autosk/state.json`. |
| `:` | Command palette. Verbs: `task new`, `task edit`, `task done`, `task cancel`, `task reopen`, `task resume`, `task enroll`, `task block`, `task unblock`, `task comment`, `session abort`, `scope clear`, `refresh`, `quit`. |
| `/` | Filter the focused panel — see [Filter syntax](#filter-syntax). |
| `*` | Clear all scope chips. |
| `R` | Force-refresh now (skip the periodic tick). |
| `ctrl+r` | Hard refresh: clear the session-transcript cache and tear down the live session stream, then re-read from `autoskd`. External writes normally arrive automatically via the daemon push — reach for this when a session's transcript looks stale or rows still don't update after pressing `R`. |
| `@` | Toggle the command-log viewport. |
| `q` / `ctrl+c` | Quit. |

Hotkey notation: plain keys are lowercase (`j`, `tab`, `enter`,
`esc`, `pgup`); uppercase letters mean shift+letter (`R` = shift+r,
`K` = shift+k); modifier chords use `ctrl+x` / `alt+enter`, and an
uppercase letter after a modifier folds shift on top (`ctrl+S` =
ctrl+shift+s). The in-app `?` cheatsheet uses the same spellings.

### Tasks `[1]`

| Key | Action |
|---|---|
| `n` | New task — two-pane compose editor (summary + description). Empty summary cancels silently. |
| `c` | **Edit** the selected task — same two-pane editor pre-filled with the current title + description. |
| `d` | Mark **done** (confirms when status was `work`). |
| `x` | Cancel (confirms). |
| `o` | Reopen (`done` / `cancel` → `new`, preserves the workflow). |
| `e` | Enroll into a workflow — opens the [two-pane workflow + step picker](#enroll--resume-picker). Flashes `no workflows defined` and skips the popup when the project has no workflows. |
| `r` | Resume (`human` → `work`) via the same picker, with the workflow pane locked to the task's current workflow. See [Enroll / resume picker](#enroll--resume-picker) for the step-selection semantics and the no-transition shortcut. |
| `b` | Add a blocker (prompts for blocker id). |
| `u` | Remove a blocker (prompts for blocker id). |
| `m` | Add a comment — single-pane multi-line compose. `enter` inserts a newline; `ctrl+s` submits; `esc` cancels. Empty submit is a silent cancel. |
| `M` | **Edit metadata** — opens `$EDITOR` on the task's metadata as pretty JSON. On save the editor diffs the document at **top-level-key granularity** and sends changed/added keys via `task.metadata.set` and removed keys via `task.metadata.unset` (never a whole-document replace). Reset a workflow visit cap by deleting `step_visits` here. Last-writer-wins against a concurrent engine `step_visits` bump (same model as a direct `task.json` edit). Also bound on the Detail pane. |
| `space` | Set the Sessions-panel scope to the selected task without leaving Tasks (the counterpart to `enter`, which scopes *and* jumps). Press `*` to clear. |
| `J` / `K` | Scroll the task-detail viewport. |

There is no priority key in v2 — tasks carry no priority field. Per-task
metadata is editable with `M` (see above).

#### Enroll / resume picker

`e` and `r` open the same two-pane popup. Left pane is the list of
workflows in the project (from the daemon's registry); right pane is
the step list of the workflow currently under the cursor.

| Pane | Key | Action |
|---|---|---|
| workflow | `j` / `k` / `↑` / `↓` / wheel | Move cursor; the step pane on the right re-renders for the highlighted workflow. No datasource call is dispatched on cursor moves. |
| workflow | `enter` | Confirm the workflow and move focus to the step pane (step cursor lands on row 0). |
| workflow | `esc` | Close the popup. No enroll / resume is dispatched. |
| step | `j` / `k` / `↑` / `↓` / wheel | Move cursor. |
| step | `enter` | Confirm the step and dispatch the call: enroll into `(workflow, step)` for `e`, resume to `step` for `r`. |
| step | `esc` | Enroll: return focus to the workflow pane, preserving its cursor. Resume (workflow pane locked): close the popup. |

**Pre-selection.** On open the workflow cursor lands on the task's
current workflow when it is present in the cached workflows slice,
otherwise on row 0. The step cursor lands on the task's current
step when present in that workflow, otherwise on row 0.

**Resume specifics.**

- The workflow pane is locked to the task's current workflow
  (single row, marked `Workflow (locked)`); focus starts on the
  step pane.
- `r` on a task with no workflow flashes
  `task has no workflow; enroll first` and does NOT open the popup.
- `r` on a task whose workflow isn't in the cached slice (stale
  cache after an external write) flashes
  `task workflow not loaded; refresh and retry`. Press `R` (or
  `ctrl+r`) and retry.
- **No-transition shortcut.** Pressing `enter` on the pre-selected
  current step resumes the task without changing step (status flip
  only); the flash reads `resumed <id> (no transition)`. Picking a
  *different* step routes through the transition path and the flash
  reads `resumed <id> -> STEP`.

Type-to-filter / fuzzy search inside the picker is not bound; the
picker is navigation-only.

### Sessions `[2]`

A **session** is one invocation of an agent's run for one task step
(the v2 replacement for the v1 daemon job).

| Key | Action |
|---|---|
| `enter` | Running session → caret jumps into the `input` textarea below the Detail pane. Terminal session → logical focus moves to the Detail pane (`j` / `k` scroll the transcript). |
| `K` | Abort session (parks the task to `human`). |

Cursor moves on a running session open a live `session.subscribe`
subscription after a short debounce so back-to-back `j` / `k`
keystrokes don't churn the stream.

### Workflows `[3]`

**Read-only** in v2. Workflows are code contributed by extensions
(discovered by the daemon), not editable DB rows — there is no
create / delete / update or isolation-edit hotkey.

| Key | Action |
|---|---|
| `enter` | Filter the Tasks panel to this workflow (scope chip `wf=<name>`) and focus Tasks. |

Isolated workflow rows render a muted `[wt]` marker after the
workflow name.

### Agents `[4]`

**Read-only.** Agents are inline step values inside workflows — there
is no separate agent registry and no install / uninstall verb in v2.
The pane lists the distinct agent steps across the project's workflows
(by step name); the Detail pane shows the agent's name.

### Detail pane (any entity)

Applies whenever the Detail pane has focus (`0`, or `enter` on a
terminal Sessions row).

| Key | Action |
|---|---|
| `j` / `k` / `↑` / `↓` | Line scroll. |
| `ctrl+f` / `ctrl+b` / `pgdn` / `pgup` | Page scroll. |
| `g` / `G` | Jump to top / bottom. |
| wheel | One line per tick. |

### Session input (running session only)

The 6-row `input` textarea pinned under the Detail pane.

| Key | Action |
|---|---|
| `ctrl+d` | Send the textarea contents to the agent as a **steer** (`session.input` with `kind=steer`) — delivered mid-turn. |
| `ctrl+f` | Send the contents as a **follow-up** (`session.input` with `kind=followup`) — queued and delivered at the start of the next agent turn. |
| `ctrl+a` | Abort the in-flight agent turn. |
| `esc` | Return focus to the Sessions panel; clear the buffer. |
| `ctrl+b` / `pgup` / `pgdn` | Page-scroll the Detail pane above without losing the input's text. |
| wheel | Scroll the Detail pane above. |

Dispatch targets the session the input was authored against — not the
current cursor. If a refresh shuffles the cursor onto a different
running session while you're typing, `ctrl+d` / `ctrl+f` / `ctrl+a`
still route to the original session. The buffer also persists while
the cursor stays on the same running session; it clears on dispatch,
on `esc`, or when you explicitly move the cursor to a different
session.

When a running session transitions to a terminal status, the textarea
disappears on the next layout pass and focus reverts to the Sessions
panel.

---

## Filter syntax

`/` opens an incremental, case-insensitive filter on the focused
panel. The filter is rendered as a chip in the bottom bar; press
`esc` to drop it.

Tasks accept structured facets followed by free text. The free text
is matched as a substring against id + title.

| Facet | Effect |
|---|---|
| `status:<status>` | Task status. One of `new`, `work`, `human`, `done`, `cancel`. |
| `wf:<name>` | Workflow name. An unknown name returns zero rows so the filter never silently widens. |

Example:

```
/wf:feature-dev refactor
```

selects tasks in `feature-dev` whose title or id contains
`refactor`. (The v1 `p:` priority and `agent:` author facets were
removed alongside those fields.)

The other panels (Sessions, Workflows, Agents) take a plain substring
query, matched against id + status + workflow + step name (or the
analogous fields per panel).

---

## Scope chips (cross-linking)

Moving the cursor in one panel updates the Detail pane and, for
some panels, narrows the others via a **scope chip** shown in the
bottom bar.

| Trigger | Effect |
|---|---|
| `enter` / `space` in **Tasks** | Sessions panel gets `scope: task=<id>` and filters to that task's runs. |
| Cursor in **Workflows** | Tasks **and** Sessions get `scope: wf=<name>` and filter to that workflow. |
| Cursor in **Sessions** | Detail pane updates only — no scope chip propagates back. |
| `*` (anywhere) | Clears every scope chip. |

Tasks no longer auto-scope on every `j` / `k` (that was noisy); set
the task scope explicitly with `space` (stay) or `enter` (scope +
jump to Sessions).

Scope is additive: `wf=X` + `task=Y` narrows Sessions to runs of task
`Y` whose workflow is `X`. Conflicting chips just produce an empty
panel — nothing throws.

---

## Markdown rendering

The Detail pane renders user-supplied markdown as formatted ANSI:

- `Task.Description` (the `description` block on a task).
- Each comment box (multi-line bodies render in full; the full
  thread is rendered, oldest at the top and newest at the bottom —
  no display cap). The Detail pane scrolls and sticky-tails, so the
  newest comment stays on screen by default and older history is
  reachable by scrolling up.
- `Workflow.Description`.
- Assistant transcript turns in the session Detail pane.

Supported constructs are stock CommonMark: ATX headings,
`**bold**` / `*italic*`, ordered and unordered lists, blockquotes,
`inline code`, fenced code blocks, links, horizontal rules. Fenced
code blocks are syntax-highlighted; the language tag picks the
lexer, and unknown / empty tags fall back to plain monospace. Raw
UTF-8 emoji passes through; `:shortname:` shortcodes are **not**
expanded.

The compose popups (`n`, `c`, `m`) are raw editors — markdown
is rendered only when reading, never while typing. Wire formats
(CLI `--json`, the daemon's JSON-RPC payloads, the on-disk
transcript) stay on raw plain text; only the TUI display layer
interprets markdown.

If the renderer fails on pathological input (deeply nested
blockquotes, very large bodies, …), the Detail pane falls back to
the raw text rather than blanking.

---

## Changelog modal

The first time you launch a release build of `autosk lazy` that you
haven't seen before, a modal popup opens over the dashboard showing
the embedded `CHANGELOG.md`. It's rendered through the same
`internal/lazy/markdown` pipeline as the Detail pane (Tokyo Night
glamour theme, syntax-highlighted fenced code blocks), so headings,
lists, and inline code all look identical to the task / workflow
descriptions.

The popup is entirely client-side to `autosk lazy`: the changelog
body is baked into the binary at build time via `go:embed`, and the
"have I seen this version yet?" bit lives in a per-user file at
`~/.autosk/state.json`. There is no network call, no daemon
interaction, no project read — the popup works on a fresh clone
and in offline CI, before any daemon is spawned.

### When the popup fires

| State | Popup body | On dismiss |
|---|---|---|
| **First run** — `~/.autosk/state.json` missing OR `last_seen_changelog` empty. | The **full** embedded `CHANGELOG.md`, every parsed entry, newest semver first. So a brand-new operator gets the complete project history once. | `last_seen_changelog` is stamped to the newest embedded version. Subsequent starts stay silent until the next release. |
| **New release seen** — embedded changelog has versions strictly newer than `last_seen_changelog`. | Only those unseen sections, newest first. | `last_seen_changelog` is stamped to the newest embedded version. |
| **Up to date** — nothing newer than `last_seen_changelog`. | No popup. | `state.json` is not touched. |
| **Dev build** — `buildinfo.Version` is `dev`, empty, or any `git describe` output that doesn't normalise to a clean `vX.Y.Z` (e.g. `v0.3.1-5-gabc1234-dirty`). | No popup, ever. | `state.json` is not touched, so a dev session can't accidentally mark a future release as seen. |
| **`--no-changelog` passed** | No popup, ever. | `state.json` is not touched. |

### Keymap (popup open)

| Key | Action |
|---|---|
| `j` / `k` / `↑` / `↓` | Line scroll. |
| `ctrl+f` / `ctrl+b` / `pgdn` / `pgup` | Page scroll. |
| `g` / `G` | Jump to top / bottom. |
| wheel | One line per tick. |
| `esc` / `enter` | Dismiss. On the auto-popup, also stamps `last_seen_changelog`. On the `ctrl+w` re-opener, dismiss is read-only. |

If you press `ctrl+w` (or open another popup — `?`, `/`, `:`)
*while* the auto-popup is on screen, the pending `state.json` stamp
is preserved or applied automatically. You can't accidentally skip
the "mark seen" step by reaching for a different hotkey first.

### Manually re-opening

- `ctrl+w` is bound globally and opens the modal with the **full**
  embedded `CHANGELOG.md`, regardless of `last_seen_changelog`. It
  does NOT mutate `state.json`, so re-reading the history any
  number of times is free.
- The `?` help cheatsheet lists the binding under the Global
  section as `ctrl+w  what's new (open CHANGELOG.md)`.

### `~/.autosk/state.json` — the seen-state store

The popup's per-user state lives in a tiny JSON file co-located
with `daemon.sock`:

```json
{
  "last_seen_changelog": "0.1.4"
}
```

- **Location.** `$HOME/.autosk/state.json`. Override with
  `$AUTOSK_STATE_FILE` (used by tests and golden runs that need to
  redirect the file without touching `$HOME`). When `$HOME` is
  unset, falls back to `./.autosk/state.json` — the same shape
  `defaultSockPath` uses for the daemon socket.
- **Permissions.** `0600` on the file, `0700` on the parent
  directory. Nothing sensitive lives here today, but the per-user
  scope means tighter perms keep weird multi-user setups
  predictable.
- **Atomic write.** Save goes through a tempfile in the same
  directory + `os.Rename`, so a crash mid-write can never leave a
  half-flushed struct on disk.
- **Missing file.** Treated as a zero `State{}` (`last_seen_changelog: ""`),
  which is exactly the "brand-new operator" first-run path.
- **Scope.** The store is operator-scoped, not project-scoped —
  reading the changelog once in any project silences the popup
  across every project on the machine.

### Authoring — how new entries get into the popup

The changelog is **manually authored**. Every PR that ships a
user-visible change (CLI verb, flag, TUI hotkey, output format,
env knob, …) extends the top-of-file `## [Unreleased]` block in
`CHANGELOG.md` following [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/);
release tagging promotes the block to a dated
`## [X.Y.Z] - YYYY-MM-DD` header.

`CHANGELOG.md` at the repo root is a relative symlink to
`internal/changelog/CHANGELOG.md`, which is the canonical file the
binary `go:embed`s. Editing either path resolves to the same file;
release tooling (`scripts/changelog-section.sh`,
`.github/workflows/release.yml`, GitHub's auto-rendered
release-notes view) keeps using the familiar repo-root path while
the `go:embed` directive sees a real file inside the package.

Version ordering inside the popup is by semver, not file order, so
`0.10.0` correctly outranks `0.9.9`. Malformed sections (bad date,
missing version) are silently skipped at parse time, the valid
neighbours survive, and the binary never panics on a broken
changelog — the popup is a UX nicety, not a release gate.

---

## Daemon dependency

`autosk lazy` is a pure JSON-RPC client of `autoskd` (which it
auto-spawns on first use) and never reads the project store itself —
there is no offline or degraded mode. The status bar's `daemon=` chip
reports the daemon's reachability:

| State | Status bar | Meaning |
|---|---|---|
| **ok** | `daemon=ok workers=N q=N r=N` | The daemon is reachable and `meta.healthz` is green. Reads, writes, live `session.subscribe` streaming, abort-session, and the `input` textarea all work. |
| **stale** | `daemon=stale` | The socket is reachable but `meta.healthz` returned an error. Panels keep their last-read contents until it recovers. |
| **down** | `daemon=down` | The daemon is unreachable (it could not be spawned, or `--sock` / `$AUTOSK_SOCK` points at the wrong path). Panels can't refresh; see [Troubleshooting](#troubleshooting). |

Panels update on the daemon's `task-changed` / `project-changed`
push, so external writes (from the CLI, another `lazy`, or the
daemon's own session activity) appear within milliseconds — there is
no client-side poll. `--refresh` only sets a long safety re-sync
interval (floored to 30s while the push is active) that re-reads
everything as a backstop in case a notification is dropped across a
daemon reconnect. Cursor moves still re-fetch the focused detail
immediately.

### Cross-process freshness

The project store is owned by `autoskd`; the Go front ends (CLI,
lazy) never read it directly. External writes — from the CLI,
another `lazy`, or the daemon's own session activity — reach lazy
through the daemon's `task-changed` / `project-changed` push,
normally within milliseconds. Press `R` to force a refresh sooner,
or `ctrl+r` to tear down the live session stream + transcript cache
and re-read from scratch when a session's transcript still looks
stale.

---

## Flags

| Flag | Default | Effect |
|---|---|---|
| `--sock <path>` | `$AUTOSK_SOCK` or `~/.autosk/daemon.sock` | Daemon UDS path. |
| `--refresh <dur>` | `2s` | Safety re-sync interval. Panels update on the daemon's `task-changed` / `project-changed` push; this value is only a backstop re-sync and is floored to 30s while the push is active. |
| `--no-changelog` | `false` | Suppress the [changelog modal](#changelog-modal) auto-popup on lazy start. Read-only — `~/.autosk/state.json` is NOT modified, so re-launching without the flag picks up where you left off. Needed for golden tests and headless CI runs. |

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `daemon=down` but `autosk session list` works | Stale socket path. Pass `--sock` or set `$AUTOSK_SOCK`. |
| No `input` textarea on a session you know is running | The live datasource flipped the session to a terminal status — the textarea only renders for running sessions. |
| Workflows / Agents panels carry no write hotkeys | Read-only by design — v2 workflows and agents are code contributed by extensions, not editable rows or installable packages. |
| `ctrl+f` does something different than I expected | Same chord, two view-scoped meanings: page-forward in the Detail pane, follow-up dispatch in the input textarea. The `?` overlay filters by focused panel, so only the meaning that's currently active is listed. |
| Detail pane shows `(loading…)` and stays there | Archive load is in flight; if it never resolves, check the daemon log or press `ctrl+r` to drop the cache and retry. `(archive load failed: …)` means the underlying fetch errored — retry with `ctrl+r`. |
| Comments for a session are missing in the session Detail | They live on the parent **task** detail. Focus the Tasks panel (`1`) and move the cursor onto the parent task. |
| External CLI writes don't show up | They normally arrive automatically via the daemon's `task-changed` / `project-changed` push. Press `R` to force a refresh; if a session's transcript is still stale, `ctrl+r` tears down the live stream + transcript cache and re-reads. |
