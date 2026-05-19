# `autosk lazy` — implementation plan (gocui rev.)

**Design contract (UX is locked here, don't relitigate):**
[`docs/plans/20260519-Lazy-Plan.md`](20260519-Lazy-Plan.md).

**Status:** implementation plan only. No autosk tasks created from
this doc yet. The previously-created task `as-78f6` carries the
bubble-tea-flavoured plan in its description and is superseded by
this document; before any code lands we'll either cancel as-78f6 and
re-create from this plan, or rewrite its description in place.

---

## 1. What changes vs. the old impl plan in as-78f6

| Topic | Old (bubble tea) | New (gocui + lazygit patterns) |
|---|---|---|
| TUI framework | `github.com/charmbracelet/bubbletea` + `bubbles/{textarea,viewport}` | `github.com/jesseduffield/gocui` (lazygit's fork — tcell/v3, Task manager, ViewMouseBinding, OnWorker). |
| Styling | `github.com/charmbracelet/lipgloss` | Keep `lipgloss` as a pure ANSI-string library (gocui views are `io.Writer`, they pass through ANSI). No port to gookit/color. |
| Root model | One `tea.Model` with state-machine | gocui `Gui` + **Context tree** (base context, list context trait, view trait, parent-context stack for popups). |
| Concurrency | `tea.Cmd` workers + typed messages | `g.OnWorker(func(Task) error)` + `g.Update(...)` back to UI thread. |
| Long-running view content (SSE, transcript pages) | Custom Bubble Tea programs | gocui **Task / ViewBufferManager** pattern (lazygit `pkg/tasks/tasks.go`). |
| Layout math | Hand-rolled width-split in `view.go` | `github.com/jesseduffield/lazycore/pkg/boxlayout` (declarative Box tree with weights/sizes/conditional direction). |
| Popups (menu/confirm/prompt/toast) | Ad-hoc Bubble Tea models | `internal/lazy/tui/popup` mirroring `pkg/gui/popup/popup_handler.go`. |
| Embedded Live tab | "Embed `internal/attach/tui` as a sub-model" | **Not possible** with gocui. `internal/attach/tui` is **retired** outright; Live tab is a new gocui context that reuses only `internal/daemon/client` (SSE + POST endpoints). |
| `autosk attach <job>` | Standalone subcommand backed by its own TUI | **Removed entirely.** The CLI verb, the package, the e2e tests, and `docs/attach.md` all go away. To open a job inspector from the command line, users run `autosk lazy --job <id>` (deep-link flag that pre-selects the job and switches straight into the inspector). |
| Search / palette / filter | Inline tea models | `SearchTrait` (per-view incremental search, lazygit pattern) + `Menu`-backed command palette + `FilteredListViewModel`-style filter on list contexts. |

Everything else from `20260519-Lazy-Plan.md` survives untouched
(layout, hotkeys, cross-linking, inspector tabs, graceful degradation).

---

## 2. Dependencies and module wiring

### 2.1 New module dependencies

| Module | Purpose | How to wire |
|---|---|---|
| `github.com/jesseduffield/gocui` | TUI framework. | Add to `go.mod`. Pin to the same commit `~/me/dev/gocui` is at. Use a `replace github.com/jesseduffield/gocui => ../gocui` during local development; CI uses the tagged version. |
| `github.com/jesseduffield/lazycore` | We use exactly one subpackage: `pkg/boxlayout` (211 LOC, no transitive deps beyond `samber/lo` which we already pull). | Add as a normal dep. *Alternative:* copy the file into `internal/lazy/tui/boxlayout/` under MIT attribution to avoid the dep. **Default: add the dep**; the file is small but it's somebody else's invariant. |
| `github.com/gdamore/tcell/v3` | Pulled transitively by gocui. | No direct use in our code. |

`github.com/charmbracelet/{bubbletea,bubbles,lipgloss}`: lipgloss
stays (we render colored strings into gocui views). bubbletea +
bubbles get removed once `internal/attach/tui` is retired (step 27
below).

### 2.2 What goes away (in full)

The attach surface is **removed completely**. There is no shim, no
alias, no deprecation period. Everything that referenced the old
command either disappears or is rewritten to point at `autosk lazy`:

- `internal/attach/tui/` — entire package deleted. The SSE plumbing
  it depended on lives in `internal/daemon/client` and stays.
- `cmd/autosk/attach.go` — deleted. The `autosk attach` cobra
  command no longer exists; running it after this change prints
  cobra's standard unknown-command error.
- `cmd/autosk/attach_e2e_test.go` and
  `cmd/autosk/attach_client_e2e_test.go` — deleted. Replaced by
  `cmd/autosk/lazy_e2e_test.go` (the gocui-backed smoke test).
- `docs/attach.md` — deleted (the runtime user-docs page). The
  historical design plans `docs/plans/20260519-Attach-Plan.md` and
  `docs/plans/20260519-Attach-Plan-v2.md` stay as archaeology;
  they describe the previous iteration and shouldn't be rewritten.
- Any README mentions of `autosk attach` — none today (verified),
  but step 29 re-checks. `docs/daemon.md` references the attach
  flow; that paragraph is rewritten to point at `autosk lazy`.

### 2.3 What we explicitly **do not** copy from lazygit

- Their `pkg/gocui/` vendor of gocui — we use the upstream
  `github.com/jesseduffield/gocui` directly.
- Their `pkg/gui/style/` package — too tied to gookit/color and a
  custom theme config; lipgloss is enough.
- Their config file mechanism (`pkg/config/`) — `autosk lazy` has
  no user config in v1.
- Their i18n / Tr.* — single-language v1, English strings inline.
- Integration test harness — Go unit tests are sufficient for
  bubbletea-shaped per-context tests.

---

## 3. Architecture overview

```
                          ┌──────────────────────────────────────┐
                          │           gocui.Gui                  │
                          │  (alt-screen, tcell, MainLoop)       │
                          └──────────────────────────────────────┘
                                          │
              ┌───────────────────────────┼────────────────────────────────┐
              │                           │                                │
        ┌─────▼──────┐            ┌───────▼────────┐               ┌───────▼────────┐
        │ Context    │            │ Controllers    │               │ Helpers        │
        │ tree       │            │ (keybindings   │               │ (refresh,      │
        │ (panels+   │            │  + handlers    │               │  popup, focus, │
        │  popups)   │            │  per context)  │               │  scope,filter, │
        └────────────┘            └────────────────┘               │  daemon health)│
              │                           │                        └───────┬────────┘
              ▼                           ▼                                │
        ┌────────────────────────────────────────────────────────────────┐ │
        │                    Datasource (compose)                        │◀┘
        │                                                                │
        │   live (uses internal/daemon/client over UDS) ───┐             │
        │   offline (uses internal/store + transcript read)│ fallback    │
        │   compose: 2s health probe, swaps verbs          │ on daemon=  │
        │                                                  │ down/stale  │
        └──────────────────────────────────────────────────┴─────────────┘
              │
              ▼
        .autosk/db  ·  ~/.autosk/daemon.sock  ·  .autosk/sessions/*/session.jsonl
```

The four boxes mirror lazygit's split:

- **Contexts** (in `internal/lazy/tui/contexts/`) — one per panel,
  popup, or main view. Each context owns its `*gocui.View`, its
  windowName for boxlayout, and a list of attached controllers.
- **Controllers** (in `internal/lazy/tui/controllers/`) — return
  `[]*Binding` and implement the handlers. A context can have many
  controllers (e.g. TasksContext has ListController, TasksWriteController,
  and GlobalController bindings attached).
- **Helpers** (in `internal/lazy/tui/helpers/`) — stateful services
  used by controllers (refresh polling, popup creation, focus
  management, scope-chip state, daemon health).
- **Datasource** (in `internal/lazy/datasource/`) — the read/write
  seam. Unchanged from the old plan: `compose.go` picks `live` or
  `offline` based on UDS health, exposing a single typed interface.

### 3.1 Context tree (concrete shape for v1)

```
Global
├─ Tasks                   (left side, window "tasks")
├─ Jobs                    (left side, window "jobs")
├─ Workflows               (left side, window "workflows")
├─ Agents                  (left side, window "agents")
├─ Detail                  (right main, window "main")
│   variants by focus: TaskDetail / JobDetail / WorkflowDetail / AgentDetail
├─ CommandLog              (right bottom, window "extras")
├─ StatusBar               (bottom, window "bottom")
│
├─ Inspector (transient)   replaces the dashboard's window arrangement when active
│   ├─ InspectorLive       (window "inspectorMain")
│   ├─ InspectorArchive    (window "inspectorMain")
│   ├─ InspectorMeta       (window "inspectorMain")
│   └─ InspectorSignals    (window "inspectorMain")
│
├─ Popups (parent-context stack)
│   ├─ Menu                (palette + per-context menus + status pickers)
│   ├─ Confirmation        (destructive verbs)
│   ├─ Prompt              (free-text input — comments, titles, paths)
│   ├─ Search              (/ overlay)
│   └─ Suggestions         (completion for prompts — e.g. id/workflow/agent pickers)
```

Total: ~16 contexts. Lazygit has 35+; we don't need most of them.

### 3.2 Two window arrangements

Layout is computed by boxlayout from a `Box` tree. We define two
top-level trees, picked by a `ScreenMode`-style enum
`viewState ∈ {Dashboard, Inspector}`:

- **Dashboard tree** — 4 left side views (stacked, focused one
  enlarged), one wide main view, command log under the main view,
  status bar at the bottom.
- **Inspector tree** — small top header (run id + tab strip), one
  large main view (the focused tab), optional bottom textarea
  (Live tab only), status bar. Side panels are hidden.

`Esc` (or `Ctrl-O`) flips `viewState` and calls `g.Update(...)`;
boxlayout re-arranges on the next frame.

---

## 4. File layout

```
internal/lazy/
├── tui/
│   ├── gui.go                 # gocui.Gui construction, MainLoop, top-level state
│   ├── layout.go              # gocui Manager: invokes boxlayout, calls SetView
│   ├── arrangement.go         # boxlayout Box trees for Dashboard + Inspector
│   ├── keybindings.go         # global bindings + collation of context bindings
│   ├── focus.go               # focus manager (Tab/1..4/0/Esc, parent-context stack)
│   ├── view_state.go          # Dashboard ↔ Inspector switcher
│   ├── contexts/
│   │   ├── base_context.go    # BaseContext (copied shape from lazygit, trimmed)
│   │   ├── list_context.go    # ListContextTrait + ListRenderer (column-aligned)
│   │   ├── filtered_list.go   # FilteredListViewModel (in-mem filter, cursor stability)
│   │   ├── search_trait.go    # incremental search across a view's content
│   │   ├── parent_stack.go    # popup parent-context manager
│   │   ├── view_trait.go      # thin wrapper over *gocui.View (focus point, viewport bounds)
│   │   ├── tasks.go
│   │   ├── jobs.go
│   │   ├── workflows.go
│   │   ├── agents.go
│   │   ├── detail.go          # dispatches by focused side context
│   │   ├── inspector_live.go
│   │   ├── inspector_archive.go
│   │   ├── inspector_meta.go
│   │   ├── inspector_signals.go
│   │   ├── menu.go
│   │   ├── confirmation.go
│   │   ├── prompt.go
│   │   ├── suggestions.go
│   │   ├── search.go
│   │   ├── command_log.go
│   │   └── status_bar.go
│   ├── controllers/
│   │   ├── base.go            # empty Binding helpers (lazygit baseController shape)
│   │   ├── global.go          # ?, :, /, R, q, Tab, 1..4, 0, Esc, *
│   │   ├── list.go            # ListController: j/k/PgUp/PgDn/Home/End/scroll
│   │   ├── tasks_writes.go    # n c d x e r b u m p o
│   │   ├── workflows_writes.go# n v D \
│   │   ├── agents_writes.go   # i u R
│   │   ├── jobs_actions.go    # a s i K m
│   │   ├── inspector.go       # [ ] 1..4 Esc (tab cycling / leave)
│   │   ├── live.go            # ctrl+d ctrl+f ctrl+a inside Live tab + textarea editor
│   │   ├── archive.go         # / n N g G y inside Archive tab
│   │   ├── palette.go         # palette open + dispatch
│   │   └── filter.go          # filter open + parse + apply
│   ├── helpers/
│   │   ├── refresh.go         # per-scope async refresh (lazygit RefreshHelper shape)
│   │   ├── popup.go           # PopupHandler: Menu, Confirm, Prompt, Toast, WaitingStatus
│   │   ├── focus.go           # focus push/pop, view highlight, side-window memory
│   │   ├── scope.go           # scope chips (Tasks→Jobs, Workflow→Tasks, etc.)
│   │   ├── filter.go          # facet parser, per-context apply
│   │   ├── daemon_health.go   # 2s healthz probe + state channel
│   │   ├── live_stream.go     # SSE → ViewBufferManager wire-up for Live tab
│   │   └── archive_pager.go   # paged messages → ViewBufferManager for Archive tab
│   ├── presentation/
│   │   ├── tasks_row.go       # one row of the Tasks panel (columns + styling)
│   │   ├── jobs_row.go
│   │   ├── workflows_row.go
│   │   ├── agents_row.go
│   │   ├── task_detail.go     # the right pane for a Task
│   │   ├── job_detail.go
│   │   ├── workflow_detail.go
│   │   ├── agent_detail.go
│   │   └── transcript.go      # shared MessageEvent → lipgloss block renderer
│   ├── style/
│   │   └── style.go           # lipgloss style accessors (one source of truth)
│   ├── types/
│   │   ├── binding.go         # Binding{ViewName,Key,Handler,Description,Tooltip,
│   │   │                      #         GetDisabledReason,DisplayOnScreen,Tag}
│   │   ├── context.go         # IContext, IListContext, ContextKey, ContextKind
│   │   ├── refresh.go         # RefreshScope, RefreshMode, RefreshOptions
│   │   └── popup.go           # CreateMenuOptions, CreateConfirmOptions, ...
│   └── *_test.go
├── datasource/                # unchanged from prior plan
│   ├── datasource.go
│   ├── live.go
│   ├── offline.go
│   ├── compose.go
│   └── *_test.go
└── runner/
    ├── runner.go              # Run(ctx, opts) + RunWithJob(ctx, jobID, opts) entry points
    └── runner_test.go

cmd/autosk/
└── lazy.go                    # `autosk lazy` cobra command (sole TUI entry point)

(removed entirely)
├── cmd/autosk/attach.go
├── cmd/autosk/attach_e2e_test.go
├── cmd/autosk/attach_client_e2e_test.go
├── internal/attach/tui/                  # whole package
└── docs/attach.md                        # runtime user docs (the design plans stay)
```

---

## 5. Patterns we adopt from lazygit (with explicit boundaries)

### 5.1 Context tree (`pkg/gui/context/`)

We copy the **shape** of `BaseContext`, `ListContextTrait`,
`ParentContextMgr`, `ViewTrait`, `SearchTrait`, and
`FilteredListViewModel`. We do **not** copy:

- patrician/dynamic-title bits;
- the 30+ context-key constants — we add ours (~16);
- range selection (single-select only in v1);
- accordion mode (focused panel grows ~40%; that's it).

Each context has:

```go
type Context interface {
    Key() ContextKey
    Kind() ContextKind          // SIDE_CONTEXT | MAIN_CONTEXT | POPUP_CONTEXT | …
    WindowName() string         // boxlayout key
    View() *gocui.View
    Keybindings() []*Binding
    HandleFocus()
    HandleFocusLost()
    HandleRender()              // re-renders into the view (called on data change)
}
```

### 5.2 boxlayout (`pkg/gui/controllers/helpers/window_arrangement_helper.go`)

We use boxlayout straight from `lazycore`. `arrangement.go` builds
two Box trees:

```go
dashboardArrangement(args) *boxlayout.Box {
    return &boxlayout.Box{
        Direction: ROW,
        Children: []*Box{
            { Direction: COLUMN, Weight: 1, Children: []*Box{
                { Direction: ROW, Weight: 1, ConditionalChildren: sidePanelChildren },
                { Direction: ROW, Weight: 2, Children: []*Box{
                    { Window: "main", Weight: 3 },
                    { Window: "extras", Size: cmdLogSize(args) }, // command log
                }},
            }},
            { Window: "statusbar", Size: 1 },
        },
    }
}
```

The focused side panel grows via `ConditionalChildren` (returns
weights with the focused window at 3, others at 1). This is the
lazygit accordion behaviour, the minimum needed to surface the
selected row.

`inspectorArrangement(args)` is simpler: a 1-line top tab strip, a
big main, an optional textarea (Live tab only), and the status bar.

### 5.3 Refresh helper (`pkg/gui/controllers/helpers/refresh_helper.go`)

Reduced to one method:

```go
func (h *RefreshHelper) Refresh(opts types.RefreshOptions) {
    // opts.Scope:  []RefreshScope{Tasks, Jobs, Workflows, Agents, Detail, Inspector}
    // opts.Mode:   ASYNC | SYNC
    for _, scope := range opts.Scope { h.refreshScope(scope, opts.Mode) }
}
```

A 2-second `time.Ticker` fires `Refresh({ Scope: All, Mode: ASYNC })`
on the UI thread (`g.Update`). Each scope's async branch runs via
`g.OnWorker(...)`, fetches via `datasource`, then posts back through
`g.Update(...)` to swap the context's underlying slice + call
`HandleRender()`. **No goroutine-per-panel.**

The focused detail re-fetches on selection change too, not just on
the tick.

### 5.4 Popup handler (`pkg/gui/popup/popup_handler.go`)

Five entry points, all backed by gocui views that get
push/popped via the parent-context stack:

```go
type PopupHandler interface {
    Menu(opts CreateMenuOptions) error                 // multi-choice with descriptions
    Confirm(opts ConfirmOpts) error                    // yes/no
    Prompt(opts PromptOpts) error                      // single-line free text
    Editor(opts EditorOpts) error                      // multi-line via $EDITOR (shell out)
    Toast(message string, kind ToastKind)              // ephemeral footer flash
    WithWaitingStatus(msg, f func(Task) error) error   // spinner on a worker
}
```

Toast goes to the command log AND the status bar's right-side flash
slot. Editor shells out to `$EDITOR` (or `EDITOR=vi`) on a temp
file — lazygit does the same for commit messages, with the same
pause/resume of the gocui loop (`gocui.Gui` has the pause/resume
hooks for this).

### 5.5 Task / ViewBufferManager (`pkg/tasks/tasks.go` + gocui Task)

This is the piece that solves "stream SSE into a view without
choppy renders or memory blow-up". Wrap a `*gocui.View` with a
small adapter:

```go
type StreamPump struct {
    view      *gocui.View
    task      gocui.Task          // pause/resume/cancel signal
    onAppend  func(api.MessageEvent) string  // → ANSI string to write
    readLines chan int            // gocui's lazy "give me more" channel
}
```

The Live tab uses StreamPump on top of `client.StreamHandle.Events`.
Archive tab uses a similar pump on top of paged
`/v1/jobs/{id}/messages?full=true&before=<cursor>` reads.

This is also lazygit's mechanism for `git show <commit>` previews
in the main panel — same pattern, different source.

### 5.6 Binding struct

```go
type Binding struct {
    ViewName        string
    Key             gocui.Key
    Modifier        gocui.Modifier
    Handler         func() error
    Description     string
    ShortDescription string         // for the bottom-line hint
    DisplayOnScreen bool            // include in bottom hint line
    OpensMenu       bool            // appends "…" to the description
    Tag             string          // for `?` cheatsheet grouping
    GetDisabledReason func() *DisabledReason
}
```

The `?` overlay groups bindings by `Tag` ("nav", "writes", "search",
"inspector") and the bottom status bar surfaces only the bindings
with `DisplayOnScreen=true` from the focused context.

### 5.7 Search trait

`SearchTrait` lives on the contexts where it makes sense (every
list context + Archive tab + Detail). Pressing `/` invokes
`SearchHelper.Open(currentContext)`. Search history is per-context.
gocui's `SetNearestSearchPosition` does the cursor placement.

For list contexts the search filters; for transcript-style contexts
it's incremental find with `n/N`.

---

## 6. Work breakdown

One commit per bullet on `feature/new-lazy`. The order is the review
surface; squashing later is fine.

1. **go.mod wiring + replace directives.** Add `gocui` and
   `lazycore`. Add a `replace` line for local `gocui` development.
   Confirm `go build ./...` and `go vet ./...` pass with no code
   changes elsewhere.

2. **datasource interface + offline impl + unit tests.** Same as
   the previous plan §1. Defines `Datasource` + types + every
   read/write verb. No daemon traffic.

3. **datasource live impl + composer + tests.** Same as previous
   plan §2. Adds `compose.go` with the 2s health probe.

4. **gocui skeleton + MainLoop + alt-screen + quit.**
   `internal/lazy/tui/gui.go` builds a `*gocui.Gui`, sets a
   `Manager` that just clears one view, binds `Ctrl-C` to
   `ErrQuit`. `runner.Run()` ties it all together. Smoke test:
   open and close the GUI inside a TTY-stub.

5. **boxlayout + dashboard arrangement.** `arrangement.go` builds
   the Box tree. `layout.go` translates dimensions to `SetView`
   calls. Four empty side views + main + extras + statusbar, all
   visible, focused panel highlighted by border colour. Test
   compares the returned dimensions map against a fixture.

6. **Context tree skeleton.** BaseContext + ListContextTrait +
   ViewTrait + ParentContextMgr. No content. Empty contexts for
   Tasks/Jobs/Workflows/Agents/Detail/CommandLog/StatusBar; one
   per file. Tests verify context.Key() / View() / WindowName()
   round-trip cleanly.

7. **Keybinding registry + global controller.** `keybindings.go`
   collates Bindings from each context's controllers. Global
   bindings: `1..4`, `Tab`/`Shift-Tab`, `0`, `?`, `q`/`Ctrl-C`,
   `Esc` (pop popup), `*` (clear scope), `R` (refresh focused
   scope).

8. **Refresh helper + 2s tick.** Implements `Refresh(opts)` on
   `g.OnWorker`. Wires the tick from `gui.go`. Empty per-scope
   bodies for now. Tests use a fake datasource that records calls.

9. **Tasks context — read.** Subscribes to `Datasource.Tasks` via
   Refresh. Default filter `open`; column layout per the design.
   Right detail renders TaskDetail. Cursor movement is via
   `ListController`. Tests: golden frames against a tmpdb.

10. **Workflows context — read + scope chip producer.** Cursor on
    a workflow emits a `scope: wf=<name>` chip; chip narrows
    Tasks. `*` clears.

11. **Jobs context — read + scope consumer.** Reads `task=` scope
    from `scope.go`; applies. Job-status glyphs include
    `Streaming` / `AttachCount`. (Note: the daemon API will need
    those exposed on `api.JobResponse` — see §10 risk #2.)

12. **Agents context — read + opt-in scope.** Cursor only emits
    scope chip on `Enter`-confirm via a Menu popup
    ("Filter Tasks by author? by current-step agent? Cancel.").

13. **Right detail composer.** `detail.go` dispatches by focused
    side context; per-entity presentation files render the body.
    Tests compare per-focus golden frames.

14. **Popup handler — Menu + Confirm + Prompt + Toast.** Each as a
    standalone gocui view pushed onto the parent-context stack.
    Editor (multi-line via `$EDITOR`) lands here too — gocui has
    pause/resume hooks for shell-out.

15. **Filter trait + `/` per panel.** Facets:
    `status:foo p:1 wf:bar step:s agent:a id:as-`. Filter chips
    render alongside scope chips. `Esc` clears.

16. **Command palette `:`.** Backed by Menu. Static verb registry
    plus per-context contributions. `:goto <id>`, `:scope clear`,
    `:filter status=done`.

17. **Writes — task lifecycle hotkeys.** `n c d x e r b u m p o`
    per design §6.2. Uses Popup for confirms/prompts/Editor.
    Errors land in command log + a Toast.

18. **Writes — workflow + agent hotkeys.** `n v D \` on
    Workflows; `i u R` on Agents.

19. **Inspector arrangement + viewState switch.** Pressing `Enter`
    on a Jobs row sets `viewState=Inspector` and pushes the four
    inspector tab contexts. `Esc` flips back. Empty tab bodies.

20. **Inspector Meta + Signals tabs.** Pure read renderers. Tab
    strip + `[`/`]`/`1..4` cycle.

21. **Inspector Archive tab + paged transcript.** Uses
    `archive_pager.go` (Task pattern) to lazy-load
    `/v1/jobs/{id}/messages?full=true` in pages. Renders via
    `presentation/transcript.go`. `/` search with `n/N`. `y`
    copies the selected event payload to clipboard (best-effort).

22. **Inspector Live tab + SSE pump.** `live_stream.go` consumes
    `client.StreamHandle.Events` and pushes pre-rendered blocks
    into the view via the Task pattern. Textarea is an
    Editable view with a custom Editor that:
    - inserts `\n` on `Enter`;
    - dispatches `Ctrl-D` (send), `Ctrl-F` (follow_up), `Ctrl-A`
      (abort) to `internal/daemon/client`.

23. **Job hotkeys + cancel-job verb.** Dashboard hotkeys
    `a/s/i/K/m` per design §6.2. `K` calls `JobCancel` and
    toasts on result.

24. **Graceful degradation — daemon-down path.** When
    `Health.Daemon != ok`: Live tab is disabled (footer hint),
    Jobs reads from offline source, status bar shows
    `daemon=down`. Test: spin up the composer against a missing
    UDS path; every read still works, Live yields
    `ErrDaemonRequired`.

25. **`autosk lazy` command + smoke E2E.**
    `cmd/autosk/lazy.go`: flags `--sock`, `--cwd`, `--refresh`
    (default 2s), `--job <id>` (deep-link: pre-selects the job
    in the Jobs panel and switches straight to the Inspector at
    the default tab — Live for non-terminal runs, Archive for
    terminal). End-to-end smoke test populates a task and a
    done job, then runs the GUI in a pty-stub and asserts the
    first frame contains the task id and the job id.

26. **Remove `autosk attach` and its assets.** Single commit
    deletes `cmd/autosk/attach.go`, the two attach e2e tests,
    `internal/attach/tui/`, and `docs/attach.md`. Update the
    cobra command registration in `cmd/autosk/main.go` so the
    verb is gone. Rewrite the attach paragraph in
    `docs/daemon.md` to reference `autosk lazy --job <id>`.
    Leave the two historical design plans under
    `docs/plans/20260519-Attach-Plan*.md` untouched (archaeology).
    Verify `grep -r 'autosk attach' . --include='*.go' --include='*.md'`
    returns nothing outside `docs/plans/`.

27. **bubbletea / bubbles cleanup.** Remove
    `github.com/charmbracelet/bubbletea` and
    `github.com/charmbracelet/bubbles` from `go.mod`. Keep
    `lipgloss`. `go mod tidy` clean.

28. **User docs — `docs/lazy.md`.** Layout diagram, hotkey
    table, daemon requirements (Live tab only), troubleshooting,
    `--job` deep-link recipe. Link from `README.md` "Status"
    section.

29. **README update + roadmap pruning.** Add `autosk lazy` to
    the command reference. Move v1 inclusions out of "Roadmap
    (post v0.2)". Re-grep for any lingering `autosk attach`
    mentions and excise them.

---

## 7. Reuse rules (non-negotiable)

- **All daemon HTTP traffic MUST go through `internal/daemon/
  client`.** The Live tab's SSE pump is the only consumer that
  needs more than one-shot requests; it uses
  `Client.Stream(...)` as-is.
- **All DB reads/writes MUST go through `internal/store` and
  existing helpers** (`cmd/autosk/storeio.go` etc.). If a helper
  is missing, lift it to `internal/store` rather than
  reimplementing in the lazy package.
- **The Datasource is the only seam between the TUI and the rest
  of autosk.** Contexts/controllers/helpers never import
  `internal/store`, `internal/daemon/client`, or
  `internal/workflow` directly.
- **No goroutine-per-panel.** All async work runs through
  `g.OnWorker(...)`; all UI mutation goes back through
  `g.Update(...)`.
- **gocui.View is the single source of truth for content.** Don't
  cache rendered strings in context state; render into the view
  on `HandleRender`. The Task / ViewBufferManager pattern is
  the exception (streamed content) and lives in helpers.

---

## 8. Required tests

| Layer | Tests |
|---|---|
| `datasource/offline` | Each read+write verb against a tmpdb. |
| `datasource/live`    | Against a fake daemon (httptest UDS). |
| `datasource/compose` | Health probe + verb routing + fallback. |
| `tui/contexts/list_context` | Cursor stability across filter/refresh; column rendering. |
| `tui/contexts/filtered_list` | Facet parse + apply + clear. |
| `tui/helpers/refresh` | Per-scope routing; SYNC vs ASYNC; cancellation. |
| `tui/helpers/popup` | Menu/Confirm/Prompt parent-context stack push/pop. |
| `tui/helpers/scope` | Tasks→Jobs, Workflow→Tasks, Agents-opt-in, `*` clear. |
| `tui/arrangement` | Dimensions map vs. golden fixtures for several terminal sizes (80x24, 120x40, 200x60, portrait 60x80). |
| `tui/controllers/inspector` | Tab cycle, Esc returns, viewState transitions. |
| `tui/controllers/live` | Editor handles Enter/Ctrl-D/Ctrl-F/Ctrl-A correctly. |
| `cmd/autosk/lazy_e2e_test.go` | pty-stub smoke test (§25). One subtest exercises the `--job <id>` deep-link path so the inspector-on-launch contract is regression-tested. |

`go vet ./...` clean. `go build ./...` clean.

---

## 9. Acceptance checklist (review surface)

- [ ] `autosk lazy` runs against a project with the daemon up.
      Tasks, Jobs (with `Streaming`/`AttachCount`), Workflows,
      Agents render. Scope chips work both ways. `/` and `:` work.
- [ ] `Enter` on a job opens the inspector at the right default tab.
      Each tab works; SSE in Live carries the same hotkey contract
      that the (now-removed) `autosk attach` had — Ctrl-D send,
      Ctrl-F follow_up, Ctrl-A abort.
- [ ] All write hotkeys in design §6.2 succeed against a tmpdb.
- [ ] With the daemon socket missing, `autosk lazy` starts, reads
      Jobs from `.autosk/db`, opens the Archive tab on a stored
      session, and the bottom bar shows `daemon=down`.
- [ ] `autosk lazy --job <job-id>` opens the inspector directly on
      that job (deep-link); the dashboard view is reachable via Esc.
- [ ] `autosk attach` is **gone**: running it prints cobra's
      unknown-command error. `internal/attach/tui/`,
      `cmd/autosk/attach*.go`, and `docs/attach.md` are all
      removed. `bubbletea` / `bubbles` are out of `go.mod`;
      `lipgloss` stays. The two historical design plans under
      `docs/plans/20260519-Attach-Plan*.md` remain as archaeology.
- [ ] `docs/lazy.md` exists and matches what the binary actually
      does. `docs/daemon.md` no longer mentions `autosk attach`.
- [ ] All tests pass; build clean.

---

## 10. Things that will derail this task and how to surface them

- **Risk 1: gocui module replace.** Local development uses
  `~/me/dev/gocui`; CI doesn't. Pin a specific commit in `go.mod`
  before merging. Comment on the task if the local checkout's
  HEAD has unreleased patches we depend on — we'll either upstream
  them or vendor.

- **Risk 2: `api.JobResponse` missing `Streaming` / `AttachCount`
  on `feature/new-lazy`.** Tests in `internal/daemon/server`
  reference these fields, but `internal/daemon/api/types.go` on
  this branch doesn't carry them. They were promised by the attach
  v2 plan but the field additions never merged into the api types.
  Land them as a prerequisite step 0 — surface them on
  `JobResponse` (already populated server-side in `server/sse.go`).

- **Risk 3: gocui textarea (Editable view) doesn't handle
  multi-line cleanly out of the box.** lazygit avoids multi-line
  in-view editing by shelling out to `$EDITOR` for commit
  messages. If the custom editor in §22 is gnarly, fall back to
  the same approach for the Live tab: `Ctrl-E` opens `$EDITOR`,
  on save the file's contents become the next dispatch buffer.
  This degrades UX slightly (no live-as-you-type) but is robust;
  flag in the task before implementing.

- **Risk 4: SSE pump under-renders during fast streams.** The
  Task / ViewBufferManager pattern in lazygit was tuned for
  shell-command output (line-at-a-time). SSE frames carry whole
  events. Apply the same `THROTTLE_TIME = 30ms` knob from
  `pkg/tasks/tasks.go` to coalesce renders, and document the
  trade-off (slight latency vs. flicker) in `docs/lazy.md`.

- **Risk 5: `$EDITOR` shell-out leaves gocui in a weird state.**
  gocui has `Suspend`/`Resume` hooks; lazygit uses them. If a
  terminal we test on (kitty? wezterm? tmux?) leaves alt-screen
  state mis-restored, document as a known issue and switch to a
  built-in single-line prompt as a fallback for the affected
  fields.

- **Risk 6: Bindings collisions.** `/` inside a Prompt context
  must NOT trigger the global search; `1..4` inside the Prompt
  must be plain text. The parent-context-stack handles this
  (popups own the keyboard while focused), but it's easy to slip
  a global binding accidentally. Add a test that exercises every
  popup with every global key.

- **Risk 7: Long-running fetches block the UI.** Datasource
  verbs MUST return quickly or be invoked from `OnWorker`. Add a
  lint-style sanity test that wraps every blocking datasource
  call in a context with a 100ms timeout in the test build, to
  catch accidental UI-thread DB hits.

---

## 11. Out of scope (explicit)

Same as `20260519-Lazy-Plan.md` §10 + §12, plus:

- We do **not** port lazygit's screen-mode (half / full / normal).
  Dashboard ↔ Inspector is enough.
- No mouse support in v1 (gocui has it; we just don't bind anything).
- No custom user config / keymap remapping.
- No theming / colour-scheme picker — lipgloss defaults only.
- No demo-mode / GIF capture tooling.
- No multi-project view (`--all-projects`). Same as before.

---

## 12. References

### Design
- [`docs/plans/20260519-Lazy-Plan.md`](20260519-Lazy-Plan.md) — UI/UX contract (unchanged).

### Existing autosk code we reuse
- `internal/daemon/client` — UDS HTTP + SSE; no changes needed.
- `internal/daemon/api` — add `Streaming` + `AttachCount` to
  `JobResponse` (risk #2).
- `internal/store` + `cmd/autosk/storeio.go` — DB-side helpers
  for writes.
- `internal/transcript` — `session.jsonl` reader; used by the
  Archive tab via datasource.

### lazygit references (read-only, do not vendor wholesale)
- `pkg/gui/context/{base_context,list_context_trait,list_renderer,
   search_trait,filtered_list_view_model,parent_context_mgr,
   view_trait}.go` — context tree shapes.
- `pkg/gui/controllers/{base_controller,list_controller,global_controller}.go`
   — controller pattern.
- `pkg/gui/controllers/helpers/{window_arrangement_helper,refresh_helper,
   popup,search_helper}.go` — helper shapes.
- `pkg/gui/popup/popup_handler.go` — popup interface.
- `pkg/tasks/tasks.go` + `pkg/gocui/task*.go` — streaming pattern.
- `pkg/gui/types/keybindings.go` — Binding struct shape.

### Upstream
- `github.com/jesseduffield/gocui` — TUI framework.
- `github.com/jesseduffield/lazycore/pkg/boxlayout` — layout math.
