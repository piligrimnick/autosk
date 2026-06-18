# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

> **Clean break — autosk v2.** The daemon is rewritten from scratch as a
> Bun/TypeScript program (`autoskd`), and the storage engine is gone: a project's
> tasks, comments, and session transcripts are now plain **files** under
> `.autosk/` — there is **no database**. v2 does **not** read the old
> `.autosk/db`, and there is **no migrator**: open an existing v1 project with the
> last v1 release, [`v0.1.6`](https://github.com/wierdbytes/autosk/releases/tag/v0.1.6).
> Workflows and agents are now **code** registered by extensions, not database
> rows or installed npm-package agents.

### Added
- **Editable task metadata.** Every task now carries a free-form `metadata`
  object in `task.json` (always present, omitted on disk when empty), readable
  and editable through a new `autosk metadata show/set/unset` verb group, the
  `task.metadata.set` / `task.metadata.unset` daemon RPC methods, a lazy TUI
  Metadata panel (`M` opens `$EDITOR`), and the GUI task edit modal; the engine
  reserves the `step_visits` sub-object (ask-93a91b).
- **GUI: iPhone-friendly compact single-pane layout.** On touch devices below
  the compact breakpoint (`(pointer: coarse) and ((max-width: 700px) or
  (max-height: 480px))`) the Tauri GUI switches to a full-screen single-pane
  shell — a top bar (project switcher + Settings gear), one active list, and a
  bottom tab bar (Tasks/Sessions/Workflows) with push-to-detail navigation, a
  ‹ Back control, a pinned contextual composer, full-screen modal sheets, and
  safe-area insets in both orientations; desktop and iPad keep the two-pane
  layout unchanged (ask-5c793f).
- **GUI: streaming partial agent messages.** An active pi-agent turn now renders
  live in the Tauri GUI — assistant text, thinking, and tool-call blocks grow as
  the model produces them, instead of appearing only after the step commits. Built
  on a new ephemeral `session-event` `kind:"partial"` frame (a cumulative,
  never-persisted snapshot that carries no transcript line and never advances the
  subscription cursor) and an SDK `ctx.partial(message)` channel; the committed
  durable line supersedes the live bubble with no flicker or duplication
  (ask-b9258c).
- **Interactive (taskless) chat sessions.** Open a session from the GUI Sessions
  panel (`＋` → NewSessionModal), pick a registered agent, and chat with the model
  turn-by-turn — the session is tied to no task and no workflow. Backed by a new
  first-class agent registry (`AutoskAPI.registerAgent`; `@autosk/pi-agent`
  registers the `pi` agent) and three new proto-v2 RPC methods
  `registry.agent.list` / `session.create` / `session.end`; a graceful End seals
  the session `done` (vs Abort → `aborted`) (ask-f330c1). A live chat now also
  surfaces its **turn activity**: the session badge reads `working` while the
  agent is streaming a turn and `idle` when it is waiting for your next message
  (instead of a bare `running`). Backed by a new `SessionMeta.activity`
  (`idle`|`busy`) field, an `AgentRunContext.setActivity` hook the chat agent
  drives from pi's `agent_start`/`agent_end`, and a `session-changed` push so
  both the Sessions panel rows and the open session view update live.
- **GUI: browse & install `autosk-extension` npm packages from the Workflows
  panel.** A new `＋` action in the Workflows header (shown only when a project is
  active) opens a browser listing npm packages published with the
  `autosk-extension` keyword, sorted by weekly downloads; each row shows
  name+version, description, weekly downloads, publisher, and last-updated date.
  Clicking a row opens the package's npmjs.com page in the default browser;
  already-installed packages show an "Installed (global/project)" badge. Install
  asks where to put it (Globally / To this project → `extension.install`
  `{ local: false | true }`) and then hints to reopen the project for the new
  workflow(s) to appear. The npm search runs in the Tauri backend (new
  `extension_search` command, scoped to the npm registry); opening pages uses the
  official `@tauri-apps/plugin-opener`, restricted to `https://www.npmjs.com/*`.
- **`--force`/`-f` on `autosk done` & `autosk cancel`:** force-reaps the task's
  isolation env (git worktree) even when it has uncommitted changes — the branch
  is always preserved, only the working dir + its uncommitted edits are dropped.
  Surfaced in all three front ends: the CLI flag, a force-confirm prompt in the
  `autosk lazy` Tasks panel, and a force-confirm dialog in the desktop GUI. New
  proto-v2 error code `ENVIRONMENT_DIRTY` (1005) backs the warn-then-force flow, and
  the `IsolationProvider` SDK contract gains an optional, identity-keyed `reap()`
  (session-free cleanup) method.
- **daemon (`autoskd`):** a brand-new Bun/TypeScript daemon that owns each
  project's `.autosk/` directory and drives tasks through their workflows. It is
  auto-spawned on first use over a Unix socket (single-instance bind; opt-in TCP
  + token for remote front ends; idle-shutdown), serves the proto-v2 JSON-RPC
  surface, and ships as a standalone binary (`bun build --compile` — embeds the
  Bun runtime, so no global `bun` is needed at runtime). Replaces the v1 Go
  `autosk daemon`.
- **live session updates:** the `autosk lazy` TUI and the desktop GUI now see
  sessions appear and change state in real time. The daemon adds a project-scoped
  `session-changed` notification (pushed on a session's create → running →
  terminal transitions) plus the `session.subscribeProject` /
  `session.unsubscribeProject` RPC verbs; front ends subscribe once per active
  project and no longer have to know a session id ahead of time — or poll — to
  watch a freshly-enrolled task's session spin up and run to completion.
- **storage:** tasks / comments / sessions are now files under `.autosk/`
  (`tasks/<id>/task.json` + `comments.jsonl`, `sessions/<id>.json` + `.jsonl`),
  written atomically by the daemon, with a startup scan + fs-watcher that picks
  up external (human/script) edits. No database, no migrations, no GC.
- **workflows & agents as code:** workflows are TypeScript registered by
  **extensions** (their agents are inline step values — the step key is the agent
  name), with pi-style discovery (project `.autosk/extensions/` ▸ global
  `~/.autosk/extensions/` ▸ npm packages in `settings.json`),
  loaded in-process with full error isolation — a broken extension is recorded
  via `project.diagnostics` and never crashes the daemon. The engine only drives
  the task status machine and calls each workflow's `onTransit` hook; visit caps
  and guards live in that hook (`ctx.visits`). See `docs/extensions.md` and
  `docs/workflows.md`.
- **extension packages (npm):** `@autosk/sdk` (the extension-facing types),
  `@autosk/worktree` (the per-task git-worktree isolation provider, attachable to
  any workflow), `@autosk/pi-agent` (an agent that drives `pi --mode rpc`, mirrors
  pi's transcript entries 1:1, and bridges transitions through an injected
  `autosk_transit` pi-tool), and `@autosk/feature-dev` (the reference workflow
  `dev → review → docs → validator → accept` with review→dev / validator→dev
  bounce-backs and a `dev` visit cap — replaces v1's `feature-dev-generic`). All
  four are published to npm as raw TypeScript; the daemon installs
  `@autosk/feature-dev` on first run (see **first-run bootstrap**).
- **`@autosk/merge-to-current` workflow (npm):** a single-step, non-isolated
  workflow that merges a task's `autosk/<task-id>` branch into the project's
  **current** branch (whatever `HEAD` points at) — the v2 port of v1's
  `merge-to-main`, with the destination changed from `main`/`master` to the
  current branch and a fast-forward-when-possible merge. It auto-commits pending
  task-worktree edits to the branch first, refuses on a dirty tree / detached
  HEAD, and either lands cleanly (`done`) or fully rolls back and parks for a
  human (`human`). Published to npm but NOT part of the first-run bootstrap —
  install with `autosk ext add npm:@autosk/merge-to-current`, then
  `autosk enroll <id> --workflow merge-to-current`.
- **first-run bootstrap:** on a fresh machine (no `~/.autosk/settings.json`) the
  daemon provisions the default extensions itself — it `npm install`s
  `@autosk/feature-dev` (deps pulled transitively) into `~/.autosk/packages/` and
  writes `~/.autosk/settings.json`, so every project discovers `feature-dev` with
  no per-project files. `settings.json`'s presence is the "already initialised"
  marker (provide your own to opt out); the install needs `npm` on `PATH`
  (`$AUTOSK_NPM_BIN`) + network and is logged-but-never-fatal on failure.
- **auto-install reconcile:** on every start the daemon also installs any package
  listed in `settings.json#extensions` that is not yet present — the global
  `~/.autosk/settings.json` once per start, each project's
  `./.autosk/settings.json` on first open. Only **missing** packages install
  (no upgrade); set **`AUTOSK_NO_AUTO_INSTALL`** to disable all automatic
  installs (first-run bootstrap included).
- **`autosk ext` (extension management):** the `autosk ext` command group —
  `autosk ext add <source>`, `autosk ext list`, and `autosk ext remove <source>`
  to manage `settings.json#extensions`, with `-l/--local` for project scope; a
  source is `npm:<spec>[@version]` (installed into the scope's packages dir) or a
  local path (`/abs`, `./rel`, `../rel`, `~/path`, referenced in place). Plus
  `autosk ext update [source]`: bump installed floating-`npm:` extensions to
  newer registry versions in place — global scope outside a project, the union
  of global + project inside one, with `--global` / `-l/--local` scope overrides
  and a `--dry-run`/`--check` preview; version-pinned (`npm:foo@1.2.3`) and
  local-path entries are reported as skipped (ask-6dfc43, ask-08d0cb).
- **sessions:** one agent run for one step is a **session** with a pi-format
  transcript (`sessions/<id>.jsonl`: a header, pi `message` entries with text /
  thinking / `toolCall` / image content blocks, and the engine's structural
  `autosk:transit` / `autosk:steer` / `autosk:error` / `autosk:session_end`
  entries). Live sessions support steer / follow-up / abort.
- **cli:** new `autosk session` group — `list [--task ID]`, `get`, `transcript
  [--from-line N] [--limit N]`, `abort`, `input <message> [--followup]`;
  the read-only `autosk workflow list|show` registry view;
  a new `autosk project` group (`list`, `add`, `diagnostics`); and
  `autosk comment edit|delete` (comments are now editable/deletable) (ask-305572).
- **gui:** a new native **Tauri desktop app** (`gui/`) at `autosk lazy` parity —
  a frameless two-panel workspace (a Tasks / Sessions / Workflows accordion + a
  polymorphic entity view driven by one entity-aware composer), a live pi-format
  session transcript (`session.transcript` / `session-event`) with steer / abort,
  editable/deletable comments, a project switcher with an extension-diagnostics
  badge (`project.diagnostics`), a read-only workflow registry view,
  whole-UI zoom, a collapsible/resizable sidebar, and a Liquid Glass app icon.
  Runs **local** (auto-spawned `autoskd` over UDS) or **remote** (TCP + token);
  the Tauri backend is a pure JSON-RPC client of `autoskd` (ask-9e5f8c,
  ask-03019a).
- **build/release:** `make build-autoskd` compiles the Bun daemon to `bin/autoskd`
  (no extension bundling); `make install` installs both `autosk` + `autoskd`.
  Releases ship **both** binaries per target, and the Homebrew formula installs
  both — so auto-spawn works on a clean machine with no global `bun`. The
  `@autosk/*` extension packages are published to npm via
  `scripts/publish-extensions.sh` (ask-305572, ask-e36027).
- **`@autosk/pi-tools` (new pi extension):** the agent-facing autosk management
  tools — `autosk_task` (create / update / show / list) and `autosk_comment`
  (add / list) — shipped as a standalone, npm-publishable pi extension over the
  `autosk --json` CLI. Install it in pi (`~/.pi/agent/settings.json`) to give an
  interactive or workflow agent a structured task/comment surface. It ships no
  transition tool: a workflow step still records its transition through the
  in-process `autosk_transit` channel.
- **`AUTOSK_CWD` project-selector override:** the `autosk` CLI now honors
  `$AUTOSK_CWD` as the project root for every verb (falling back to the working
  directory). `@autosk/pi-agent` sets it — to the canonical project root, also
  exposed to agents as `ctx.projectRoot` — when it spawns a workflow agent, so
  `autosk` calls from inside a **worktree-isolated** run (the tools above, or a
  bare `autosk` in a shell) resolve the task's own project instead of walking up
  from the throwaway worktree to the wrong (or no) `.autosk/`.

### Changed
- **Workflow visit counts are now metadata-backed.** `ctx.visits(step)` reads
  the persistent, human-resettable `metadata.step_visits[step]` counter the
  engine bumps on every transition into a named step, instead of counting
  session files; in-flight tasks effectively reset their counters to 0 on
  upgrade, and a cap (e.g. feature-dev's `dev`) can be lifted with
  `autosk metadata unset <id> step_visits` (ask-93a91b).
- **desktop GUI — quieter session controls:** End/Abort no longer raise an info
  notice (`End sent to session …` / `Abort sent to session …`); the session
  status flips once the daemon settles it, and errors still surface.
- **desktop GUI — wider sidebar resize:** the sidebar can now be dragged out to
  half the window width (the cap is dynamic instead of a fixed 480px), so the
  Tasks / Sessions / Workflows lists get more room on wide displays.
- **desktop GUI — calmer typography:** status badges (Tasks / Sessions panels
  and the center task/session views) now render lowercase (`work`, `running`,
  `done`, …) and are centered in their fixed-width gutter; panel titles, section
  headers, field labels, menu labels, transcript role labels, and the task
  comments header drop the all-caps treatment for sentence case (`Tasks`,
  `Sessions`, `Workflows`, `Comments`, `Assistant`, …).
- **enroll / resume — looser status gates + GUI consolidation:** `task.enroll`
  is now accepted from `new`, `cancel`, **and** `human` (previously `new` only)
  and takes an optional starting `step` (default: the workflow's first step), so
  it can place a task at any step of any workflow; `task.resume` is now accepted
  from `human` **and** `cancel` (previously `human` only). `work` (abort the live
  run first) and `done` (use `reopen`) are still rejected. The desktop GUI drops
  the separate **Resume** button: a single **Enroll** button opens the picker
  pre-seeded to the task's current workflow + step (continue where it left off)
  and always calls `task.enroll` with the picked step. The CLI `autosk enroll`
  gains a `--step STEP` flag.
- **`@autosk/sdk` isolation lifecycle:** the `IsolationProvider` contract is now
  status-driven — `acquire` stays per-step (idempotent ensure-ready), `release`
  becomes an optional, no-arg quiesce-on-exit hook fired only when a task LEAVES
  `work` (never step→step), and durable teardown moves to the optional `reap` on
  a terminal transition. No operator-visible change — the worktree provider
  behaves identically end-to-end (dir kept across steps/park, removed on
  terminal, branch preserved) (ask-35186d).
- **`@autosk/feature-dev`:** the shipped reference workflow's `validator` step
  now performs mandatory **release hygiene** on success (updates `CHANGELOG.md`
  `[Unreleased]` per Keep a Changelog, then commits a clean worktree with
  Conventional Commits) before transitioning to `accept`, and the `docs` step
  now explicitly defers `CHANGELOG.md` to `validator`. Every role prompt also
  gained an **Available transitions** section naming its intended
  `autosk_transit` targets (ported from the customized `feature-dev-generic`
  workflow that lived in v1 `.autosk/db`).
- **workflows:** workflow extensions now register their agents **inline** as
  step values — each step is either an `AgentDefinition` (the step key is the
  agent name) or a `statusStep`, and the single `registerWorkflow` API registers
  the workflow together with its agents (there is no `registerAgent`). The new
  `statusStep("done"|"cancel"|"human")` SDK helper replaces a step's
  `{ human: true }` marker and adds terminal `done` / `cancel` steps; the
  `registry.workflow.*` wire shape carries a per-step
  `status: "done"|"cancel"|"human"|null` (agent steps are `null`) instead of the
  old `agent` + `human` fields. See `docs/workflows.md` and `docs/extensions.md`.
- **protocol:** the CLI, lazy TUI, and Tauri GUI now speak the **proto-v2**
  namespaced JSON-RPC surface (`meta.*`, `project.*`, `task.*` incl.
  `task.comment.*`, `registry.*`, `session.*`) carrying only the `{cwd}` selector
  (the v1 `db_path` selector and the source discriminator are gone). The wire
  types live once in `daemon/sdk/src/proto.ts` and are mirrored by the Go
  (`internal/daemon/api`) and Tauri (`gui/src-tauri`) clients (ask-305572,
  ask-03019a).
- **tasks:** the task shape **drops `priority`, `author`, and `metadata`** (incl.
  `step_visits`) and now carries `workflow` / `step` names; `--json` output and
  the `list` table drop the priority column accordingly (ask-305572).
- **cli:** the Go `autosk` binary is now a pure JSON-RPC client — CGO-free
  (`make build` is plain `go build`), it links no storage engine of its own, and
  every verb routes through the auto-spawned `autoskd`; `autosk version` reports
  `backend: autoskd` (ask-305572).
- **lazy:** the Jobs pane is now the **Sessions** pane (one session = one agent
  run; transcript reads the pi-format `session.transcript` / `session-event`
  stream), the Workflows pane is **read-only** (workflows are code), the priority
  column / author filter / metadata editor are gone, and panels refresh on the
  daemon's `task-changed` / `project-changed` push (ask-305572).
- **bootstrap:** a freshly-created project lays down `.autosk/{tasks,sessions,
  extensions}` and registers with the daemon — no database, and no per-project
  workflow seeding (`feature-dev` is provisioned once, globally, on the daemon's
  first run — see **first-run bootstrap** — and is then available to every
  project).

### Removed
- **agents (inline-step redesign):** the `autosk agent list/show` CLI, the
  `enroll` / `create` `--agent` flag (and the `single:<agent>` synthetic
  workflow it materialised), the `registry.agent.list` RPC verb + the `AgentInfo`
  wire type, and the `AutoskAPI.registerAgent` method — agents are now inline
  workflow-step values (see Changed).
- **storage:** the `.autosk/db` database and everything tied to it — the `--db`
  flag / `AUTOSK_DB` selector and the schema/`migrate` machinery; a project is
  now just an `.autosk/` directory resolved by walk-up from `{cwd}` (ask-305572).
- **cli:** the verbs with no proto-v2 equivalent — the whole `metadata`, `sql`,
  and `worktree` groups; `gc`, `migrate`, `history`; `workflow create|delete|update`;
  `agent install|uninstall`; `step next` (and the bare `next`-as-transition
  alias); the whole `autosk daemon` group (auto-spawn + `autosk session` /
  `autosk project` replace it); and the `-p/--priority` flags + `--author` filter
  (ask-305572).
- **tasks:** `priority`, `author`, and `metadata` (incl. `step_visits`).
- **agents/workflows:** the npm-package agent system (`autosk agent install`) and
  the JSON/DB workflow editor, both superseded by code-as-extensions; the four v1
  agent npm packages under `agents/` and the example workflow JSON under
  `docs/examples/workflows/` are removed.
- **extensions (v1):** the `@wierdbytes/pi-autosk` pi extension is retired (its
  core verb `autosk_step` → `step next` no longer exists; transitions are
  daemon-internal via the in-process `autosk_transit` pi-tool), along with the v1
  custom-agent SDK + bootstrapper (`@autosk/agent-sdk`, `@autosk/agent-runtime`)
  — superseded by `@autosk/sdk` + `@autosk/pi-agent`.
- **daemon internals:** the v1 Rust daemon workspace and its embedded
  database/GC/migration machinery are gone, replaced by the Bun `autoskd` (this
  intermediate Rust daemon never reached a release).
- **daemon-bundled extensions:** there is no longer a fourth, lowest-priority
  "daemon-bundled" discovery source, no extensions packaged beside the binary,
  the `$AUTOSK_BUNDLED_EXTENSIONS` env override, and the `scripts/bundle-extensions.sh`
  bundler — the reference `feature-dev` workflow is now an npm package the daemon
  installs on first run (see **first-run bootstrap**).

### Fixed
- **GUI (iPhone compact layout): on-screen keyboard handling.** iOS WebKit
  (Safari / the Tauri WKWebView) ignores the `interactive-widget=resizes-content`
  viewport hint the compact layout relied on, so the keyboard overlaid the
  `100dvh` shell: the task/comments scroll body was cut off too early, a large
  dead gap sat between the composer and the keyboard, an empty scroll region
  appeared below the input (WKWebView's keyboard contentInset), and focusing the
  input auto-zoomed the page (the sub-16px field tripped iOS's focus-zoom),
  panning it sideways. Now: every text control is pinned to 16px (plus
  `maximum-scale=1` in the viewport meta) so focus never zooms; the document and
  shell heights are driven off the live `visualViewport.height` (a `--app-vh`
  custom property) so the content fits exactly above the keyboard with nothing
  for WKWebView to scroll (the top bar can no longer be dragged off-screen); the
  window is pinned to the top edge; and the composer drops its home-indicator
  inset while the keyboard is up. The iOS keyboard's form-accessory bar (the
  prev/next chevrons + Done checkmark) is also removed via the Tauri window
  config (`disableInputAccessoryView: true`).
- **GUI (iPhone compact layout): horizontal scroll & cramped session rows.**
  On the phone single-pane layout, opening a task or session no longer opens a
  sideways scroll: a scroll pane's `overflow-y:auto` was computing `overflow-x`
  to `auto`, so a single long unbreakable token (a path/URL) — or a fenced code
  block — dragged the whole detail screen sideways. Detail panes now pin
  `overflow-x:hidden` and hard-wrap long words and code. The Sessions list also
  stops scrolling sideways and now lets the session id shrink/ellipsis so the
  task id (or, for a chat, the agent name) prints in full on the right edge.
  Finally, the open-session header drops the duplicate session id (already shown
  in the top bar) and reads `status · workflow:step · agent` with the task id
  pinned to the right edge on one line. Desktop and iPad are unaffected (every
  rule is gated behind the compact media query).
- **pi-agent: garbled final transcript line on session end.** When a `pi`
  session exited it left an unreadable `pi-agent:warn` entry
  (`pi:stderr: \u001b[?2026h\u001b[r…`) as the last line of the transcript. `pi`
  writes a burst of terminal-teardown control sequences to stderr on exit (mouse
  tracking off, leave the alt-screen, end synchronized output) even under
  `--mode rpc`, and the driver forwarded that line verbatim. pi-agent now strips
  ANSI/terminal control sequences from forwarded stderr and drops a line that is
  pure control noise, while still surfacing real stderr text (e.g. crash stack
  traces).
- **GUI: confirmation dialogs did nothing.** The project switcher's remove
  (×) button, comment delete, session abort, and force done/cancel relied on
  the native `window.confirm()`, which this Tauri/wry build never shows on macOS
  (it returns `false` with no dialog) — so the actions silently bailed out. They
  now use an in-app confirm modal that works on every platform.
- **overlapping session status badge:** in the desktop GUI's Sessions panel, a
  row that transitioned `queued → running` briefly showed both badges stacked on
  top of each other until you hovered the row. React reused the same `<span>`
  for the badge across the status change, and the `running` badge's `.is-live`
  pulse animation promotes that node to its own WebKit (WKWebView) compositing
  layer — leaving the previous badge's pixels painted on top until a repaint
  (e.g. the row's hover background) cleared them. The badge is now keyed on the
  status, so React mounts a fresh node on each status change and the pulse
  starts on a clean layer.
- **composer horizontal scroll:** the desktop GUI's shared chat-style input
  (the *Add a comment* and *Steer the agent* composers) no longer shows a stray
  horizontal scrollbar. The auto-growing textarea only had its `overflow-y`
  managed, leaving `overflow-x` at the `<textarea>` default of `auto`, so a long
  unbreakable string (e.g. a URL) — or the vertical scrollbar appearing past 10
  lines — could trigger a sideways scroll. The field now pins `overflow-x:
  hidden` and wraps long words (`overflow-wrap: break-word`).
- **orphaned worktree on a manual done/cancel:** marking a task `done`/`cancel`
  by hand (CLI `autosk done`/`cancel`, the `autosk lazy` Tasks panel, or the
  desktop GUI) used to leave its git worktree (e.g. one a human-park step had
  kept on disk) stranded under `~/.autosk/worktrees/` forever — only the
  workflow-engine's own terminal transition reaped it. A manual terminal now
  reaps the task's worktree too, by its deterministic `(projectRoot, taskId)`
  identity, **preserving the branch** (the work survives for review/merge). If
  the worktree has uncommitted changes the verb is refused with a warning
  (`ENVIRONMENT_DIRTY`) instead of silently discarding them; re-run with `--force`
  (`autosk done -f` / `cancel -f`, or confirm the TUI/GUI force prompt) to remove
  the worktree and discard the changes.
- **daemon double-bind race on a stale lock:** `autoskd`'s single-instance
  guard could let two daemons bind the same socket when a stale lock left by a
  crashed/killed daemon was reclaimed by several auto-spawns at once. The stale
  lock reclaim had a TOCTOU between the dead-holder check and the unlink, so a
  sibling that had already reclaimed + bound could have its live lock and socket
  deleted and bound over — stranding clients on an orphaned listener and leaving
  two engines owning one `.autosk/`. Stale-lock reclamation is now serialised
  behind an exclusive breaker file (with a liveness re-check under the breaker),
  so exactly one daemon ever binds.
- **empty task list with `--status all` / in the lazy dashboard:** an "all
  statuses" request was sent as `status: []`, which the daemon's membership
  filter read as "match none" — so `autosk list --status all` and the `autosk
  lazy` Tasks panel (which always asks for every status) came back empty. The Go
  client now omits the status key when no positive status is requested, and the
  daemon treats an empty status list as "no constraint" (all statuses).
- **session list ordering:** `session.list` now returns sessions newest-first
  (by id, descending) by default — previously oldest-first — so the `autosk lazy`
  Sessions panel and the CLI render the most recent run at the top without any
  client-side re-sort.
- **transcript / task auto-scroll:** the desktop GUI no longer yanks the center
  panel to the bottom on every update regardless of where you were reading. The
  GUI now matches the `autosk lazy` TUI: selecting a session in the Sessions
  panel anchors its transcript at the newest line, opening a task starts from the
  top (title/description), and new lines/comments only auto-scroll into view when
  you are already parked at the bottom. While tailing, the GUI glides to the
  newest line with a smooth scroll animation (instant when the OS prefers reduced
  motion) instead of jumping abruptly, and a deliberate scroll-up mid-animation
  releases the tail so a streaming transcript never traps the reader. The TUI
  task detail gained the same tail-when-at-bottom behaviour (it previously never
  followed new comments).

## [0.1.6] — 2026-06-08

### Changed
- **bootstrap:** the `feature-dev-generic` workflow now ships with
  `isolation: worktree` by default.

### Fixed
- **daemon:** fix the empty lazy transcript and `HTTP 410 session_missing` from
  `daemon messages <job>`.

## [0.1.5] — 2026-05-25

### Added
- **lazy:** two-pane workflow + step picker for `enroll` / `resume` actions.
- **lazy:** redesigned Workflow Detail pane.
- **lazy:** `?` opens a sectioned, filterable cheatsheet popup; Enter runs the highlighted binding (ask-ed8035).
- **lazy:** context-aware bindings hint row pinned across the bottom of the dashboard (ask-ed8035).
- **lazy:** changelog modal on first run of a new release; `ctrl+w` to re-open the full CHANGELOG.md; `--no-changelog` to suppress the auto-popup (ask-911ea0).

### Changed
- **lazy:** status bar collapsed to a single row with ` | ` separators; legacy `?=help` hint moved into the cheatsheet popup (ask-ed8035).
- **docs:** `CHANGELOG.md` now lives at `internal/changelog/CHANGELOG.md` (embedded into the binary via `go:embed` for the new changelog modal); the repo-root `CHANGELOG.md` is a relative symlink so release tooling, GitHub's auto-renderer, and `scripts/changelog-section.sh` keep working unchanged (ask-911ea0).

### Fixed
- **lazy:** fix cursor jumping to a different row when a refresh re-sorted or re-filtered a panel.
- **lazy:** hydrate the job transcript on focus change to the Jobs panel.

### Docs
- **lazy:** document the two-pane enroll / resume picker.
- **lazy:** describe the redesigned Workflow Detail pane.
- **lazy:** document the changelog modal, `ctrl+w` re-opener, `--no-changelog` flag, and the `~/.autosk/state.json` `last_seen_changelog` field (ask-911ea0).

## [0.1.4] — 2026-05-24

### Added
- **enroll:** allow enrolling tasks that are currently in `human`, `done`,
  or `cancel` status (previously enroll was limited to `new` / `work`).
- **lazy:** rework Jobs panel columns — worktime column, colored status glyph,
  right-aligned task id.

### Changed
- **cli:** drop `autosk assign` in favour of `autosk enroll --agent`.
  `enroll --agent <pkg>` is now the single way to bind an agent to a task,
  with or without a workflow.
- **examples:** move workflow JSON examples to `docs/examples/workflows/`.

### Fixed
- **lazy:** render the full comment thread in the Tasks → Detail pane
  (drop the previous "last 5 comments" cap).
- **enroll:** address review remarks on the human / done / cancel enroll path.

### Docs
- **enroll:** refresh `docs/workflows.md` and CLI docstrings for the new
  human / done / cancel enroll behaviour.

## [0.1.3] — 2026-05-23

### Added
- **workflow:** `autosk workflow update --isolation <mode>` to change a
  workflow's worktree isolation mode in place; lazy surfaces this via the
  new `i` binding on a selected workflow.

### Fixed
- **workflow update:** per-task confirmation body, JSON-on-error output, and
  sentinel-preserving error returns (review remarks WU1–WU4).

### Docs
- Cover `workflow update --isolation` and the lazy `i` binding.
- Simplify the workflow-creation doc; layout fix for the workflow doc.

## [0.1.2] — 2026-05-23

### Docs
- Rewrite `docs/lazy.md` for the current version of the TUI.
- Add the lazy-mode dashboard screenshot (`docs/lazy-mode.png`) to the docs.
- Clean up `README.md`, add more cross-links to the docs.

## [0.1.1] — 2026-05-23

### Added
- **README:** Homebrew install option (`brew install wierdbytes/autosk/autosk`).

### Changed
- **ci/release:** automatically bump the `wierdbytes/homebrew-autosk` formula
  on every tag publish.
- **ci/release:** bump `actions/download-artifact` v6 → v7 (Node.js 24).

## [0.1.0] — 2026-05-23

Initial public release of **autosk** — a local-first task manager and
workflow engine for AI coding agents. One DB per repo, one daemon per host,
one TUI to see it all.

### Added

#### Core / task tracker
- doltlite-backed `.autosk/db` per repository.
- Task ids in the canonical `ask-XXXXXX` format (6 hex chars).
- Blockers / blocked-by graph, priorities `0..3` (`0` highest).
- Short, persisted task / run status vocabulary: `new`, `work`, `human`,
  `done`, `cancel`.
- CLI verbs: `init`, `create`, `update`, `show`, `list`, `ready`, `done`,
  `cancel`, `enroll`.
- `--step NAME` flag on `create` / `enroll` to join a workflow at a specific
  step instead of its `first_step`.
- Implicit auto-init in fresh directories on the first write verb; opt-out
  with `--skip-bootstrap`; non-interactive auto-accept via
  `AUTOSK_AUTOINIT_*` env knobs.
- Auto-bootstrap of the canonical `feature-dev-generic` workflow on
  `autosk init` (and on implicit auto-init).

#### Workflow engine
- v0.2 step engine with directed-graph workflows, transitions, agents, and
  comments.
- `autosk_step next` as the canonical agent-side "I'm done with this step"
  signal.
- Per-step `agent.params` overrides for npm-packaged agents.
- Step visit limits + target-step parking on advance failures.
- Parked / failed runs render uniformly as `[id]: name` and surface all
  fields.
- v0.3 worktree isolation per workflow; executor auto-recovers a missing
  worktree dir; `AUTOSK_DB` is isolated in tests.

#### Agents
- npm-package agent system (replaces the earlier `.autosk/agents/*.toml`).
- Reference workflow `feature-dev-generic` (dev → review → docs → validator
  → human) shipped via `go:embed` from `internal/bootstrap`.

#### Daemon
- `autosk daemon` — multi-project orchestrator over a Unix-domain socket
  (one daemon per host, project resolved per request).
- HTTP API with closure verification and SSE event stream.
- `pi`-runner reader switched to `json.Decoder`; honours the cancellation
  signal on read-loop errors; `step_signal` honoured on wait-error recovery.

#### Lazy TUI (`autosk lazy`)
- gocui-based dashboard with Tasks / Jobs / Workflows / Agents / Detail
  panes and a smoke E2E.
- Datasource layer split into offline + live + compose.
- Lazygit-style task-compose popup editor; key bindings: `c` (edit
  title / description, two-pane compose), `m` (single-pane comment compose),
  `M` (JSON-validated metadata compose), `i` (workflow isolation), `?`
  (help), `n` (new task).
- Tokyo Night theme + dedicated `theme` package; rounded panel and popup
  corners; scroll-off-margin viewport that follows the cursor; mouse-wheel
  scroll across panels and inspector.
- Tasks panel redesigned with entity colours, spinner, job marker, and
  manual-scope toggle.
- Detail pane: labeled boxes, markdown rendering with a bounded LRU cache,
  signals box stacked as a 4-column table, blockers partitioned by status,
  full comment thread.
- Job Detail redesigned — Inspector removed, sticky-tail on the transcript,
  job-input nested inside Detail and gated on `Streaming`.
- Jobs panel: age column moved leftmost.

#### Extension (`@wierdbytes/pi-autosk`)
- TypeScript SDK + agent runtime, callable from `pi --mode rpc`.
- Mega-tool split into three focused tools: `autosk_task`,
  `autosk_comment`, `autosk_step`.

#### Docs
- End-user `README.md` (install, quick start, lazy mode, CLI walk-through).
- `docs/daemon.md`, `docs/lazy.md`, `docs/workflows.md`.
- Time-rendering policy: human-facing times route through
  `internal/timeformat` in the operator's local TZ; machine wire formats
  (JSON CLI, HTTP API, `RunContextSeed`, TS types) stay on RFC3339 UTC.
- `AGENTS.md` rewritten as a repo orientation guide.

#### Build & CI
- Go 1.25 toolchain; mandatory `-tags libsqlite3` build tag routed through
  doltlite-provided libsqlite3.
- `make doctor` checks the doltlite library; every build / test target
  depends on it.
- Auto-fetch `libdoltlite`; CI runs on linux + macOS.
- Publish release binaries on every semver tag push.
- Migrations 001..007 squashed into a single release migration.

### Changed
- **statuses:** shortened persisted task / run status vocabulary
  (e.g. `in_workflow` → `work`); CLI / TUI / docs all updated.
- **tasksvc:** unified human-driven status transitions across the CLI and
  the lazy TUI.

### Fixed
- **doltlite:** validate the file inode on every connection checkout to
  close a write-loss race.
- **doltlite:** rotate the `sql.DB` connection periodically to recover from
  cross-process garbage collection.
- **runstore:** retry on `database is locked` for every write, not just
  reads.
- **workflow:** `REINDEX workflows` after `Delete` to clear phantom
  unique-index entries.
- **migrations:** rebuild `tasks` / `task_deps` in migration 007 to dodge a
  doltlite UPDATE-PK quirk.
- **compactor + lazy:** bound chunk-store WAL replay so `autosk lazy`
  doesn't peg a CPU core.
- **lazy/tui:** assorted scope, focus, and sticky-tail fixes on the Detail
  pane; widen input visibility to `!IsTerminal`; pin overlay sticky-tail
  and popup z-order; clear `TextArea` (not just `v.lines`) on
  dispatch / cursor-move / Esc.

[Unreleased]: https://github.com/wierdbytes/autosk/compare/v0.1.6...HEAD
[0.1.6]: https://github.com/wierdbytes/autosk/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/wierdbytes/autosk/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/wierdbytes/autosk/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/wierdbytes/autosk/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/wierdbytes/autosk/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/wierdbytes/autosk/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/wierdbytes/autosk/releases/tag/v0.1.0
