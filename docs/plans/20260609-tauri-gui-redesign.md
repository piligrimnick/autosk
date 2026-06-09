# Tauri GUI Redesign — 3-Panel Frameless Shell

> Status: **Plan / ready for implementation.**
> Supersedes the Phase-4 layout of
> [`20260607-Rust-Daemon-Tauri-GUI.md`](20260607-Rust-Daemon-Tauri-GUI.md) §6 (UI
> regions only — the daemon / IPC-discipline / transport contract from that plan
> is unchanged and reused verbatim).
> Reference app: `~/me/dev/CodexMonitor` (frameless chrome, animated status
> dots, design tokens, popover primitives). Parity target for behaviour:
> `autosk lazy` (see [`docs/lazy.md`](../lazy.md)).

---

## 1. Goal

Replace the current task-centric GUI (top-nav + projects→tasks sidebar +
task-timeline center + metadata right panel) with a **frameless, 3-panel
workspace shell**:

```
┌────────────────────────────────────────────────────────────────────────┐
│  ◯◯◯  titlebar strip (drag region)            ⦿ conn   ⚙ gear   _ ▢ ✕   │  ← full-width
├──────────────┬──────────────────────────────────────┬────────────────────┤
│  Sessions    │  Project ▾                  <obj_id>  │  Tasks             │
│              │                                        │  ───────────────   │
│  ◉ session   │   (polymorphic body for the selected  │  (task rows,       │
│  ◉ session   │    entity: task | session | workflow  │   lazy-style)      │
│  ◉ session   │    | empty)                            │                    │
│  …           │                                        │  ───────────────   │
│              │                                        │  workflows         │
│              │  ┌──────────────────────────────────┐  │  ◦ wf-1 [wt]       │
│              │  │ unified composer (entity-aware)  │  │  ◦ wf-2            │
│              │  └──────────────────────────────────┘  │                    │
└──────────────┴──────────────────────────────────────┴────────────────────┘
   left panel              center panel                     right panel
```

---

## 2. Locked decisions (from requirements interview)

| # | Decision | Choice |
|---|----------|--------|
| 1 | **Sessions panel** | Flat list of **all Jobs** in the active project, newest first (mirrors lazy's Jobs panel). *Future:* will also host interactive sessions with no parent task/job/workflow — model the selection as a generic "session", currently backed by a Job. |
| 2 | **Panel cross-linking** | **Independent** for now. Sessions, Tasks, Workflows do not scope/filter each other. Linking logic defined later. |
| 3 | **Task center view** | **Task sheet only**: title, description, comments (markdown), + comment box at the bottom. No transcript here. |
| 4 | **Composer** | **One unified, entity-aware composer**, reused in task + session views. |
| 5 | **Composer resolution** | **By selected entity.** Task → comment / enroll / resume / reopen (task-status driven, even if a job runs). Session → steer / follow-up / abort / cancel for a running job; **read-only** when the job is terminal. |
| 6 | **Workflow center view** | **Read-only** pretty-JSON view + step/isolation summary, plus the existing create / delete / isolation-toggle actions. True in-place editing deferred ("refine later"). |
| 7 | **Agents / Settings / project mgmt** | Project switcher **dropdown** in the center header hosts add / init / remove. **Gear** (Settings) + **connection indicator** live on the **titlebar strip**. Agents + Settings open as **modals/overlays**. |
| 8 | **Window chrome** | **Thin full-width titlebar strip** above the 3 panels (drag region; holds window controls + gear + connection dot). |
| 9 | **Frameless OS** | **macOS + Windows both first-class.** macOS = `titleBarStyle: Overlay` + `trafficLightPosition`; Windows = `decorations:false` + custom caption controls. |
| 10 | **Build approach** | **Restructure** into a feature-folder layout (à la CodexMonitor) and extend the store with a richer **selection model**. Keep the proven IPC/events/transport layer. |
| 11 | **Styling** | **Adopt CodexMonitor's design tokens + key primitives** (popover, panel, status dot); restyle autosk on top. |
| 12 | **Panel sizing** | **Fixed proportions** for now (no draggable resizers). |

---

## 3. Data-model mapping

autosk has **no standalone "session" entity** today. The closest is a **Job**
(`wire::Job`) — one run of a task, carrying `pi_session_id`, `session_path`,
`status`, `streaming`, transcript (`job.messages` + `job.subscribe`).

- **Sessions panel rows == Jobs** (`job.list(cwd, {})`, newest first by
  `created_at`). Status dot derives from `job.status` (`queued` / `running` /
  `done` / `failed` / `cancelled`) + `job.streaming`.
- **Selection model** is generalised so a future non-task session slots in
  without another rewrite:

```ts
// state/selection.ts
export type Selection =
  | { kind: "none" }
  | { kind: "task"; taskId: string }
  | { kind: "session"; jobId: string }      // currently backed by a Job
  | { kind: "workflow"; name: string };
```

`view: MainView` and the standalone `activeTaskId` are **removed** from the
top-level state and replaced by `selection: Selection`. Agents/Settings stop
being "views" and become modal flags (`ui.modal: "agents" | "settings" | null`).

---

## 4. Window chrome (frameless)

### 4.1 `gui/src-tauri/tauri.conf.json`

```jsonc
"windows": [
  {
    "title": "autosk",
    "width": 1280,
    "height": 800,
    "minWidth": 860,          // 3 panels need more min-width than today's 720
    "minHeight": 540,
    "dragDropEnabled": true,
    "titleBarStyle": "Overlay",      // macOS: native window, hidden title, overlaid traffic lights
    "hiddenTitle": true,
    "trafficLightPosition": { "x": 14, "y": 18 },
    "transparent": true              // macOS rounded corners + custom bg
  }
]
```

### 4.2 Per-platform decorations (Rust)

`titleBarStyle`/`trafficLightPosition`/`transparent` are macOS-only and do not
make Windows frameless. In `gui/src-tauri/src/lib.rs` `setup`:

```rust
#[cfg(windows)]
{
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.set_decorations(false);   // frameless on Windows
    }
}
```

(Keep `decorations: true` in the config so macOS keeps the native traffic
lights via `titleBarStyle: Overlay`.)

### 4.3 Titlebar component (`features/layout/components/Titlebar.tsx`)

- Root element `data-tauri-drag-region` + CSS `-webkit-app-region: drag`.
- **macOS**: reserve a left inset (~`72px`) so the strip never sits under the
  traffic lights. Detect OS via `@tauri-apps/plugin-os` or a small
  `navigator.userAgent` helper (`features/layout/utils/platform.ts`).
- **Windows**: render `WindowCaptionControls` (port from CM —
  `getCurrentWindow().minimize()/toggleMaximize()/close()`), right-aligned,
  `-webkit-app-region: no-drag` on each button.
- Center/right of the strip: app brand, **connection dot** (`daemon.connected`
  → `conn-ok`/`conn-down`, reuse today's TopBar logic + `reconnect()`), and the
  **gear** button (opens the Settings modal). Optionally an **Agents** button
  (or fold Agents into the gear menu).
- All interactive children get `-webkit-app-region: no-drag`.

> Port `WindowCaptionControls.tsx` + its test from CM nearly verbatim; swap the
> `isWindowsPlatform()` import for autosk's platform helper.

---

## 5. New source layout (feature folders)

```
gui/src/
├── main.tsx
├── App.tsx                         # AppShell mount + StoreProvider + modals
├── types.ts                        # unchanged (wire mirror)
├── services/
│   ├── ipc.ts                      # unchanged + 1 new helper (§7)
│   └── events.ts                   # unchanged
├── state/
│   ├── store.tsx                   # extended effects (selectSession, refreshSessions)
│   ├── reducer.ts                  # + selection slice, sessions order
│   ├── selectors.ts                # + session selectors, composer-mode-by-entity
│   ├── selection.ts                # Selection union (new)
│   └── types.ts                    # AppState: selection + ui.modal + sessionOrder
├── features/
│   ├── layout/
│   │   ├── components/AppShell.tsx          # the 3-panel + titlebar CSS grid
│   │   ├── components/Titlebar.tsx
│   │   ├── components/WindowCaptionControls.tsx   (port from CM)
│   │   ├── components/PanelHeader.tsx       # shared panel header chrome
│   │   └── utils/platform.ts
│   ├── sessions/
│   │   ├── components/SessionsPanel.tsx     # left panel
│   │   ├── components/SessionRow.tsx
│   │   └── components/SessionStatusDot.tsx  (animated; CM-derived)
│   ├── center/
│   │   ├── components/CenterPanel.tsx       # header + polymorphic body + composer
│   │   ├── components/CenterHeader.tsx      # ProjectSwitcher + entity id
│   │   ├── views/TaskView.tsx               # sheet: title/desc/comments
│   │   ├── views/SessionView.tsx            # transcript (single job)
│   │   ├── views/WorkflowView.tsx           # read-only JSON + summary + actions
│   │   ├── views/EmptyView.tsx
│   │   ├── components/Composer.tsx          # unified, entity-aware
│   │   └── components/Transcript.tsx        # extracted MessageRow renderer
│   ├── tasks/
│   │   ├── components/TasksPanel.tsx        # right-panel TOP
│   │   ├── components/TaskRow.tsx
│   │   └── components/NewTaskModal.tsx      (move existing)
│   ├── workflows/
│   │   ├── components/WorkflowsPanel.tsx    # right-panel BOTTOM
│   │   ├── components/WorkflowRow.tsx
│   │   └── components/CreateWorkflowModal.tsx (move existing)
│   ├── projects/
│   │   ├── components/ProjectSwitcher.tsx   # dropdown (switch + add/init/remove)
│   │   └── components/AddProjectModal.tsx   (move existing)
│   ├── agents/components/AgentsModal.tsx    (wrap existing AgentsView)
│   ├── settings/components/SettingsModal.tsx (wrap existing SettingsView)
│   └── shared/
│       ├── Modal.tsx, Markdown.tsx, NoticeBar.tsx   (move existing)
│       ├── Popover.tsx                      # CM PopoverPrimitives-derived
│       └── primitives.tsx                   # StatusBadge/PriorityDot/Spinner/EmptyState/Section
└── styles/
    ├── ds-tokens.css                (port from CM)
    ├── ds-panel.css, ds-popover.css, ds-modal.css   (port relevant rules)
    ├── shell.css                    # the 3-panel grid + titlebar
    └── … per-feature css
```

> The IPC-discipline guard (`scripts/check-ipc-discipline.mjs`) and the eslint
> `no-restricted-imports` rule are unchanged: `ipc.ts` stays the only `invoke`
> site, `events.ts` the only `listen` site. **Exception to note:**
> `WindowCaptionControls` calls `@tauri-apps/api/window` (`getCurrentWindow`),
> not `invoke`/`listen`, so it does not violate the guard — but confirm the
> eslint rule's allowlist doesn't block `@tauri-apps/api/window`.

---

## 6. State changes (detail)

### 6.1 `state/types.ts`

- Remove `view: MainView`, `activeTaskId`.
- Add:
  ```ts
  selection: Selection;                 // §3
  ui: { modal: "agents" | "settings" | null };
  sessionOrderByProject: Record<string, string[]>;  // job ids, newest first
  ```
- Keep `jobs`, `messagesByJob`, `seenEventId`, `subscribedJob`, `extrasByTask`,
  `byProject`, etc. unchanged.

### 6.2 `state/reducer.ts`

- New **selectionSlice**: `selection/set`, and on `project/select` reset
  `selection` to `{ kind: "none" }`.
- New `sessions/loaded` action → fills `sessionOrderByProject[root]` and upserts
  jobs into the normalized `jobs` map.
- `job/upsert` (already wired via job-events) must also **prepend** new job ids
  to `sessionOrderByProject[active]` when unseen, so a freshly-spawned run
  appears at the top of the Sessions panel live.
- Modal slice: `ui/modal`.

### 6.3 `state/store.tsx` (effects)

- `refreshSessions(root?)`: `ipc.jobList(cwd, {})` → `sessions/loaded`. Called on
  project select, on `task-changed`, on reconnect, and as a backstop. (Live job
  status/streaming already arrive via `subscribeJobEvents`.)
- `selectSession(jobId)`: set selection; ensure the job's transcript is loaded
  (terminal → `job.messages` snapshot; running → `job.subscribe` live tail).
  **Generalise the existing live-tail logic** (currently keyed to the *active
  task's* running job in `subscribeLive`) so it can also tail the
  *selected session's* job. Keep "one live subscription at a time" for v1:
  the subscribed job is whichever the user is actively viewing (session view) —
  otherwise fall back to the task's running job when a task is selected.
- `selectTask(id)` / `selectWorkflow(name)`: set selection; `selectTask` keeps
  today's `refreshTask` (loads task + jobs + comments + signals). The center
  task view no longer interleaves transcripts, so it only needs the task +
  comments; jobs/signals stay loaded for the right-panel activity dots.
- `openModal(kind)` / `closeModal()`.

### 6.4 `state/selectors.ts`

- `selectedTask`, `selectedSessionJob`, `selectedWorkflow` derived from
  `selection`.
- `sessionsForProject(state)`: `sessionOrderByProject[active].map(id => jobs[id])`.
- `composerMode(state)` reworked to be **entity-driven** (§8.4):
  - selection.kind === "session" → `"steer"` if job running/queued else
    `"readonly"`.
  - selection.kind === "task" → existing `human|new|enrolled|terminal` (drop the
    auto-`running` branch — running no longer overrides the task composer).
  - else → `"none"`.
- Keep `taskActivityMap` for the right-panel Tasks rows.

---

## 7. IPC / events impact

**No new RPCs required.** Reuse existing shims:

- Sessions panel: `jobList(cwd, {})` (already exists — currently only called
  with filters). No proto change.
- Session view transcript: `jobMessages` + `jobSubscribe`/`jobUnsubscribe`
  (exist).
- Composer session actions: `jobInput`/`jobAbort`/`jobCancel` (exist).
- Workflow read-only view: `workflowGet`/`workflowList` (exist).
- Project switcher: `projectList`/`projectAdd`/`projectInit`/`projectRemove`
  (exist).

The events layer (`subscribeJobEvents`, `subscribeTaskChanged`,
`subscribeProjectChanged`, `subscribeDaemonStatus`) is unchanged — it already
keeps the job map live, which now also feeds the Sessions panel.

> **Deferred (decision #6):** true workflow editing needs a `workflow.update`
> RPC in `autosk-core` + `autosk-proto` + `autoskd` + a Go/TS mirror. Out of
> scope for this redesign; tracked in §13.

---

## 8. Component specs

### 8.1 Left — Sessions panel (`features/sessions`)

- Header: "Sessions" (+ a refresh affordance; reuse `PanelHeader`).
- Body: `sessionsForProject(state)` → `SessionRow` list, newest first.
- `SessionRow`:
  - `SessionStatusDot` (animated) — map `job.status`:
    - `running` + `streaming` → `processing` (amber pulse, CM `@keyframes pulse`),
    - `running`/`queued` → `processing`/`ready` (queued = steady),
    - `done` → `ready` (green steady),
    - `failed` → red steady,
    - `cancelled` → muted/grey.
  - Primary line: `workflow:step` + agent (fallbacks when synthetic).
  - Secondary line: short job id + relative time + parent task id (muted).
  - Selected state highlights the row (`selection.kind==="session" && jobId`).
- Click → `effects.selectSession(jobId)`.
- **Port** CM's `.thread-status` + `@keyframes pulse` CSS into
  `SessionStatusDot` styles.

### 8.2 Center — header (`features/center/CenterHeader`)

- Left: **ProjectSwitcher** — button showing active project name + ▾; opens a
  `Popover` menu (CM-derived) listing registered projects (switch on click),
  a divider, "Add / init project…" (opens `AddProjectModal`), and per-row
  "Remove from registry". Empty registry → "Add a project" CTA.
- Right: the **selected entity id** (`obj_id` in the wireframe): task id /
  short job id / workflow name / nothing when `none`.
- The header row participates in the drag region only in its empty space
  (interactive children = `no-drag`); primary drag handle remains the titlebar.

### 8.3 Center — polymorphic body

| `selection.kind` | View | Contents |
|---|---|---|
| `task` | `TaskView` | Sheet header (id, `StatusBadge`, blocked flag, priority), `title`, `description` (markdown), then the **comments thread** (markdown, oldest→newest, sticky-tail) — lazy-parity task sheet **minus transcripts**. |
| `session` | `SessionView` | The single job's transcript via `Transcript` (extracted from today's `TaskTimeline` `MessageRow`): job header (id, status glyph, `workflow:step`, agent, timestamps, attach/corrections), then transcript events oldest-first, live-tailed if running. |
| `workflow` | `WorkflowView` | Read-only: header `name [wt]?`, description (markdown), steps list (lazy-style `<step> agent=… next=…`), isolation chip, and a **pretty-printed JSON** block (read-only `<pre>`). Action row: isolation toggle, delete (reuse `WorkflowsView` logic). |
| `none` | `EmptyView` | Empty placeholder (reserved for future non-task interactive sessions). |

- `Transcript` is the shared renderer; `TaskTimeline.tsx` is **retired** (its
  comment/signal interleave moves into nothing — comments now render in
  `TaskView`; signals are dropped from the center, still visible via lazy/CLI).

### 8.4 Center — unified composer (`features/center/Composer`)

Single component; mode from `composerMode(state)` (§6.4):

| Mode | Trigger | UI / actions |
|---|---|---|
| `steer` | session selected, job running/queued | textarea + **Steer** (⌘/Ctrl+↵), **Follow-up**, **Abort**, **Cancel job** — port today's `RunningComposer`. Routes to `selectedSessionJob.job_id`. |
| `readonly` | session selected, job terminal | no input; a muted "job is <status>" strip. |
| `new` | task selected, status `new` | enroll picker (workflow/agent + step) — today's `EnrollComposer`. |
| `human` | task selected, status `human` | comment + resume(--to step) — today's `HumanComposer`. |
| `enrolled` | task selected, status `work` | comment box — today's `CommentComposer`. |
| `terminal` | task selected, status `done`/`cancel` | reopen + (optional comment) — today's `TerminalComposer`. |
| `none` | nothing selected | composer hidden. |

> Net effect: today's `Composer.tsx` is split — its **running** branch moves to
> the session view; its task branches stay for the task view. The component file
> stays single (decision #4) and switches on `composerMode`.

### 8.5 Right — Tasks panel (top) (`features/tasks`)

- `PanelHeader` "Tasks" + **+ New task** (opens `NewTaskModal`).
- Tasks grouped by status in `STATUS_ORDER` (reuse `groupByStatus`), each
  `TaskRow` lazy-style: `PriorityDot`, task id, run/streaming indicator (from
  `taskActivityMap`), blocked flag, title, `workflow:step` subline.
- Click → `effects.selectTask(id)`. Selected row highlighted when
  `selection.kind==="task"`.
- **Per-task write verbs** (done/cancel/priority/edit/metadata/block/unblock,
  reopen/resume/enroll) move to a **row context menu** (right-click /
  kebab → `Popover`) and/or the task view header, since the standalone
  RightPanel metadata pane is gone. Reuse the existing handlers from
  `RightPanel.tsx` / `Composer.tsx`.

### 8.6 Right — Workflows panel (bottom) (`features/workflows`)

- `PanelHeader` "workflows" + **+ Create** (opens `CreateWorkflowModal`).
- `WorkflowRow`: `◦ <name> [wt]?` (synthetic rows never carry `[wt]`).
- Click → `effects.selectWorkflow(name)` → center `WorkflowView`.
- Fixed vertical split with Tasks (e.g. tasks ~62% / workflows ~38%); no
  draggable divider (decision #12).

### 8.7 Modals (`features/agents`, `features/settings`)

- Wrap the existing `AgentsView` / `SettingsView` bodies in `Modal`. Open from
  the titlebar gear (Settings) and an Agents control (gear menu or its own
  button). No behaviour change to their internals.

---

## 9. Design system adoption

- **Port** `ds-tokens.css` from CM (CSS custom properties: surfaces, borders,
  text, accent, status hues). Re-map autosk's existing class colors onto the
  tokens so the whole app shares one palette.
- **Port** the popover primitive (`PopoverSurface` + `PopoverMenuItem`) into
  `features/shared/Popover.tsx` for the ProjectSwitcher + task row menus.
- **Port** `.thread-status` + `@keyframes pulse` (status dot) and the
  `ds-panel` panel chrome.
- Keep `Modal`, `Markdown`, `NoticeBar` (move into `features/shared`, restyle
  with tokens).
- New `styles/shell.css`: the CSS grid
  `grid-template: "titlebar titlebar titlebar" auto / <left> 1fr <right>` with a
  second row for the panels; fixed left/right column widths
  (e.g. `260px … 320px`), responsive min via `minWidth` in window config.

---

## 10. Behaviour parity checklist (vs `autosk lazy`)

- [ ] Sessions list shows all project jobs, newest first, live status dots.
- [ ] Selecting a session streams its transcript live (running) or snapshot
      (terminal); steer/follow-up/abort/cancel work and route to the right job.
- [ ] Task view shows title/description/comments (markdown) + comment box;
      enroll/resume/reopen reachable.
- [ ] Tasks panel groups by status with priority/run/blocked indicators; new
      task; all write verbs reachable (row menu / task header).
- [ ] Workflows panel lists workflows with `[wt]` marker; create/delete/
      isolation toggle; center shows read-only definition.
- [ ] Project switcher: switch / add / init / remove.
- [ ] Connection indicator + reconnect; daemon pushes refresh all panels without
      manual refresh.
- [ ] Frameless chrome correct on macOS (traffic lights inset) and Windows
      (custom caption controls); titlebar drags the window.

---

## 11. Phasing (suggested PR breakdown)

1. **Shell + chrome.** `tauri.conf.json` + Rust per-OS decorations + `AppShell`
   grid + `Titlebar` + `WindowCaptionControls` + ds-tokens port. Panels are
   placeholders. (Visible frameless window.)
2. **State refactor.** Selection model + reducer/selectors/store changes; modal
   slice. No UI behaviour yet beyond wiring.
3. **Sessions panel + Session view + Transcript extraction.** Live status dots,
   transcript streaming, steer composer.
4. **Center task view + unified composer.** Task sheet + comments + entity-aware
   composer; retire `TaskTimeline`.
5. **Right panel (Tasks + Workflows) + Workflow read-only view.** Row menus for
   write verbs.
6. **Project switcher + Agents/Settings modals + NoticeBar reposition.**
7. **Polish + parity pass + tests + docs/CHANGELOG.**

---

## 12. Testing

- **vitest (pure logic):** selection reducer transitions; `composerMode`
  resolution table (§8.4) per entity/status; `sessionsForProject` ordering;
  session live-tail subscribe/unsubscribe switching between task-running-job and
  selected-session-job.
- **IPC discipline:** `npm run typecheck` must still pass (single invoke/listen
  sites); confirm the guard tolerates `@tauri-apps/api/window` usage in
  `WindowCaptionControls`.
- **Backend:** `cd gui/src-tauri && cargo check` (per-OS decorations cfg).
- Port `WindowCaptionControls.test.tsx` from CM.
- Manual GUI smoke (display-dependent; not in CI per existing README caveat):
  frameless on both OSes, live streaming, composer round-trips.

---

## 13. Out of scope / future

- **Panel cross-linking** (Sessions ↔ Tasks scope chips) — decision #2, later.
- **Interactive sessions** with no parent task/job/workflow — the selection
  model already reserves `kind: "session"`; needs a daemon-side session concept.
- **Workflow editing** — add a `workflow.update` RPC (`autosk-core` +
  `autosk-proto` + `autoskd` + Go/TS mirror) then upgrade `WorkflowView` from
  read-only JSON to an editor (form, then visual graph).
- **Resizable / persisted panel widths** — decision #12, later.

---

## 14. Docs / changelog

- Update `gui/README.md` Layout/Architecture sections for the 3-panel shell +
  selection model + frameless chrome.
- `CHANGELOG.md` (`## [Unreleased]`): **Changed** — "GUI redesigned to a
  frameless 3-panel workspace (sessions / center / tasks+workflows)". This is a
  user-visible output-format change, so an entry is required.
