# `autosk lazy` — Job Detail redesign (Inspector removal)

**Status.** Shipped.

**Predecessors.**

- [`20260519-Lazy-Plan.md`](20260519-Lazy-Plan.md) — original design
  with a fullscreen `Live / Archive / Meta / Signals` inspector.
- [`20260520-Lazy-Impl-Plan.md`](20260520-Lazy-Impl-Plan.md) —
  implementation plan that stood up that inspector.

This document records the redesign that **replaced the fullscreen
inspector with an in-pane Job Detail surface**, why we did it, what
moved where, and the contract the surviving code holds. It's the
operator-facing changelog for the redesign as well as a pointer for
the next contributor.

---

## Why

The fullscreen Inspector served a single role — render a job's
transcript + accept Ctrl-D / Ctrl-F / Ctrl-A input — at the cost of
a modal context switch the operator triggered with `Enter` and
backed out of with `Esc`. Two friction points dominated:

1. **Two surfaces for one thing.** A job's metadata was already
   reachable from the dashboard (the right Detail pane showed the
   focused task's `─ recent jobs ─` summary), then the operator had
   to press `Enter` to see the transcript, then `Esc` to get back.
   The same data lived in two places with a modal gate between
   them.
2. **Inconsistent visual language.** Comments in the task detail
   were rendered as `drawLabeledBox` per entry; the inspector's
   Live and Archive tabs rendered the transcript as a flat
   indented stream. Operators reading both surfaces had to switch
   mental models.

The redesign folds the transcript into the existing Detail pane
using the same `drawLabeledBox` per-event language operators already
know from comments. Selection-driven navigation replaces the
`Enter` / `Esc` modal dance.

---

## What changed

### User-visible

1. **No fullscreen Inspector.** `Enter` on a Jobs row no longer
   pushes a modal. Cursor moves alone surface the transcript in the
   Detail pane.
2. **`Enter` routes by status.** Running job → caret moves into a
   6-row `input` textarea pinned under Detail. Terminal job →
   logical focus moves to the Detail pane so `j`/`k`/`g`/`G`/page
   keys scroll the transcript.
3. **Job header + per-event boxes.** The Detail pane now carries a
   structured header (jobID + status glyph + workflow:step + agent
   + timestamps + attached/corrections/pid + session/error) plus
   one `drawLabeledBox` per transcript event. Assistant events run
   through markdown; every other kind renders as plain text.
4. **`--job <id>` CLI flag removed.** The deep-link existed to
   open the Inspector at launch. With the Inspector gone, focus
   the Jobs panel and move the cursor onto the job.
5. **Help screen rewritten.** No `inspector:` section. New
   `detail:` and `job input (running job):` sections.
6. **Sticky tail.** When the operator is at the bottom of the
   Detail pane and a new live event arrives, the viewport scrolls
   to follow. When scrolled up, the viewport stays put across
   live appends + refresh redraws.

### Wire formats unchanged

The daemon HTTP / SSE surface (`internal/daemon/api`,
`/v1/jobs/{id}/messages`, `/v1/jobs/{id}/sse`) is untouched. The
TUI continues to consume `Messages(jobID, full=true, 0)` for the
archive and `StreamLive(ctx, jobID)` for the live tail. No new
daemon API surface was introduced.

---

## Architecture

### Data flow

```
cursor moves on Jobs row
  → afterCursorMove(panelJobs)
      → scheduleJobLive(jobID, running):       2s debounced
          → openJobLive → pumpJobLive → state.jobTranscript[id].events
      → scheduleJobArchive(jobID, running):    immediate; per-job freshness gate
          → loadJobArchive → Messages(full=true) → mergeArchiveAndLive →
            state.jobTranscript[id].events / .renderedBoxes
  → layout pass
      → renderJobDetail(j, te, innerWidth(winDetail))
          → header rows
          → for each te.events[i] → drawLabeledBox(label, body, contentW)
          → cached in te.renderedBoxes (rebuilt only when contentW changes
            or len mismatches)
```

### Per-job cache

`state.jobTranscript` is `map[string]*jobTranscriptEntry`. Entries
carry the merged event slice, the pre-rendered box strings (keyed
to the last-rendered width), an `err` field for archive failures,
a `truncated` flag for live-cap overflow, a `loadedAt` timestamp
for terminal-TTL refetch, and a `touchedAt` timestamp for LRU
eviction.

- **Cap.** 32 entries (`jobTranscriptCacheMax`). Least-recently-
  touched eviction on overflow (scan the map for `min(touchedAt)`;
  N≤32 keeps this trivially cheap).
- **Terminal TTL.** Terminal entries whose `loadedAt` is older than
  30 s (`jobTranscriptTerminalTTL`) refetch on next selection.
  Running entries are kept fresh by SSE alone.
- **Running→terminal transition.** Detected in
  `applyRefreshLocked` by snapshotting the previous job slice
  before the swap and comparing per-jobID. Triggers an immediate
  archive refetch so the final flushed events appear in the
  cache; also reverts `state.focused` from `panelJobInput` to
  `panelJobs` so the operator doesn't end up on a deleted view.
- **Live cap.** Each entry's event slice is capped at 2000
  (`jobLiveBufCap`); on overflow the oldest 25 % are dropped in a
  single allocation and `te.truncated` is set. Re-fetching the
  archive (which uses `full=true, limit=0`) drops the truncation
  flag once the full set arrives.

### SSE lifecycle

Exactly one Live subscription at a time. The state tracks the
streamed jobID, the `LiveHandle`, the cancel func, and a debounce
timer (`jobLiveDebounce = 2 s`). `scheduleJobLive` atomically
stops the existing timer and arms the new one in a single
critical section (timer is non-blocking under the state lock).
`stopJobLive` is idempotent and called from: quit,
`afterCursorMove` when the cursor leaves panelJobs / lands on a
terminal job, `applyRefreshLocked` when the streamed job vanishes
from the jobs slice, and `handleForceReconnect`.

### Input ownership

`state.jobInput` holds the textarea contents; `state.jobInputOwner`
holds the jobID the buffer is authored against.

- **Ownership stamping.** `liveEditor` stamps `jobInputOwner` only
  when it is currently empty (first keystroke of a new draft).
  Subsequent keystrokes don't re-attribute, so the authored target
  survives a refresh-driven cursor shift to a different job.
- **Dispatch target.** `liveDispatch` (`Ctrl-D` / `Ctrl-F`) and
  `liveAbort` (`Ctrl-A`) read `jobInputOwner` first, falling back
  to the current cursor only when no draft is in flight. A
  silent refresh-driven cursor shift cannot reroute the operator's
  authored text.
- **Clear semantics.** `liveDispatch` / `jobInputEscape` /
  `clearJobInputIfStale` use `v.ClearTextArea()` (not just
  `v.Clear()`) so the underlying gocui `TextArea` is wiped too.
  Without this the next keystroke re-rasterises the previous
  draft into the visible buffer.
- **Cross-job clear.** Triggered by `afterCursorMove(panelJobs)`
  on explicit `j`/`k` moves. Refresh-driven cursor shifts
  intentionally preserve the draft (the operator never authored
  the shift; clearing here would silently punish them for the
  datasource's emit order).

### Focus model

A synthetic `panelJobInput` panelID was introduced so the layout
pass's `SetCurrentView(state.focused.window())` re-asserts
`winJobInput` as current on every redraw. Without it the next
spinner tick (~100 ms) would rip focus back to the Jobs panel and
keystrokes would no longer reach the input view.

`panelJobInput` is **not** a Tab cycle target. `cyclePanel`
normalises `panelJobInput → panelJobs` before the modulo so Tab
steps off to the next side panel (predictable from "I'm working
the selected job, the cycle starts from Jobs").

`panelDetail` similarly is not a Tab target. `cyclePanel`
normalises it through `state.detailFocus` (the side panel that
owned the selection when the operator pressed `0`); fallback is
`panelTasks` when `detailFocus` is also `panelDetail`.

### Sticky-tail math

The Detail pane's sticky-tail check runs **before** the buffer
clear / rewrite inside `writeView` and is opt-in for `winDetail`
only (Tasks / Jobs / Workflows / Agents manage origin via the
highlight loop). Math uses `v.InnerSize()` (frame-excluded inner
height), not `v.Size()` — gocui's `oy` indexes into the inner
content area. The same correction applies to `detailScrollPage`,
`detailScrollTo`, and `scrollViewByLines`.

### Markdown rendering

Assistant events (`strings.HasPrefix(string(ev.Kind), "assistant")`)
run through `markdown.Render(ev.Text, contentW)` — same renderer
the Tasks-detail pane uses for `Task.Description`,
`Workflow.Description`, and comments. The prefix check
auto-includes any future `assistant_*` kinds without code
changes.

---

## Removed surface

The package no longer carries any of:

- `StateInspector` (the `ViewState` enum keeps a single
  `StateDashboard` value for forward-compat).
- `inspectorState`, `inspectorTab`, `tabLive / tabArchive /
  tabMeta / tabSignals`.
- Windows `winInspectorHdr`, `winInspector`, `winInspectorIn`.
- Inspector-specific functions: `openInspector`,
  `openInspectorAtTab`, `hydrateInspectorTab`, `inspectorClose`,
  `inspectorCycleTab`, `inspectorJumpTab`, `inspectorScroll*`,
  `renderInspectorBody`, `renderInspectorHeader`,
  `renderInspectorSignals`.
- Inspector-specific Jobs-panel shortcuts (`a` Live, `s` Archive,
  `i` Meta).
- The `--job <id>` CLI flag on `autosk lazy` and the
  `tui.Options.InitialJob` field.

`grep -nR "inspector" internal/lazy/tui` returns only narrative
comments referencing the prior design — no live code references.

---

## Acceptance criteria (recap)

Mirrored from the task description, verified by tests in
`internal/lazy/tui/` (notable test files:
`render_job_detail_test.go`, `job_transcript_cache_test.go`,
`job_live_debounce_test.go`, `sticky_tail_test.go`,
`job_detail_layout_test.go`, `refresh_apply_test.go`):

- Header carries jobID + status glyph + workflow:step + agent;
  meta row with created/started/finished smart-formatted
  timestamps; attached/corrections/pid row; muted `session:` row;
  `error:` row in error style when non-empty.
- Per-event boxes; assistant events use markdown; other kinds
  render plain. Empty `Text` renders as just the label + frame.
- No signals / no comments-since-run in job detail.
- `winJobInput` allocated below `winDetail` (6 rows) iff the
  selected job is running.
- `Enter` running → focus `winJobInput`; `Enter` terminal →
  focus `panelDetail`.
- `Ctrl-D` send / `Ctrl-F` follow_up / `Ctrl-A` abort / `Esc`
  return + clear / `Ctrl-B` / `PgUp` / `PgDn` scroll Detail
  above.
- Detail scroll keys (`j`/`k`/arrows/`Ctrl-F`/`Ctrl-B`/`PgDn`/
  `PgUp`/`g`/`G`/wheel) work for both task-detail and job-detail.
- Sticky tail when at bottom; viewport preserved when scrolled
  up; first-frame starts at bottom.
- 2 s debounce on SSE; archive load immediate but coalesced per
  job; per-jobID cache (cap 32, LRU eviction); cache invalidation
  on terminal-TTL, running→terminal transition, and `Ctrl-R`.
- Resize rebuilds per-event boxes for the new contentW.
- Frame for 5000 cached events ≤ 50 ms (`go test -bench`
  `renderJobDetail`); spinner ticks reuse `renderedBoxes`;
  burst of 50 live events coalesces via `pumpLiveLoop` throttle.
- `make test`, `make build`, `make vet`, `make lint` clean.
- `docs/lazy.md` reflects the new flow; help screen no longer
  mentions Inspector.

---

## Out-of-scope follow-ups

Pulled forward from the task spec — not in this redesign:

- Transcript kind filter (`hide tool_call`).
- In-transcript search prompt.
- Detail-pane redesigns for `panelWorkflows` / `panelAgents`.
- Splitting the Detail viewport into fixed header + scrollable
  transcript (current behaviour is acceptable because the header
  is short relative to typical transcript size).
- Persisting `state.jobInput` across `q`.
