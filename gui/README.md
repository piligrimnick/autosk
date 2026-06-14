# autosk-gui

Tauri desktop GUI for [autosk](../README.md), at feature parity with
`autosk lazy`. The frontend is a React + Vite app; the Tauri (Rust) backend is
a **pure JSON-RPC client of `autoskd`** (the Bun daemon) — it links no storage
engine of its own. It talks to the daemon over a Unix-domain socket in
**local** mode (auto-spawning `autoskd` when absent) or over TCP + token in
**remote** mode. The frontend is transport-agnostic: only the Rust backend
switches.

> Status: **P8 (proto v2)** of the Bun-daemon rewrite — the GUI is cut over to
> the namespaced proto-v2 JSON-RPC surface (`meta.*` / `project.*` / `task.*` /
> `task.comment.*` / `registry.*` / `session.*`). Canonical plan:
> [`docs/plans/20260612-Bun-Daemon-Extensions.md`](../docs/plans/20260612-Bun-Daemon-Extensions.md)
> (§3.2 sessions/transcript, §4 RPC v2, §6 P8). Bundling `autoskd` as a sidecar
> and CI release gates remain follow-ups.
>
> The desktop UI is a **frameless two-panel workspace** — a left sidebar that
> stacks Tasks / Sessions / Workflows as a lazygit-style accordion, and a main
> panel hosting the polymorphic entity view + composer. (Earlier design notes
> for the superseded 3-panel layout live in
> [`docs/plans/20260609-tauri-gui-redesign.md`](../docs/plans/20260609-tauri-gui-redesign.md).)

## Layout

```
gui/
├── package.json                # scripts; React/Vite/Tauri deps
├── vite.config.ts              # `@` → ./src alias
├── vitest.config.ts            # pure-logic unit tests (no browser/daemon)
├── scripts/
│   └── check-ipc-discipline.mjs   # guard: single invoke + single listen site
├── src/                        # React frontend (feature-folder layout)
│   ├── main.tsx, App.tsx       # App mounts the frameless two-panel AppShell
│   ├── types.ts                # wire types (mirror daemon/sdk/src/{types,proto,transcript})
│   ├── services/
│   │   ├── ipc.ts              # the ONLY `invoke` site (typed shim per proto-v2 RPC method)
│   │   └── events.ts           # the ONLY `listen` site (ref-counted fan-out hub)
│   ├── state/
│   │   ├── selection.ts        # entity-selection union (task|session|workflow|none)
│   │   ├── reducer.ts          # normalized, slice-composed reducer
│   │   ├── selectors.ts        # sessions, transcript, entity-driven composer mode
│   │   ├── store.tsx           # effects + event router: notifications → actions
│   │   └── types.ts            # AppState shape + action union
│   ├── features/               # one folder per UI domain
│   │   ├── layout/             # AppShell, Titlebar, WindowCaptionControls, PanelHeader
│   │   ├── sessions/           # SessionsPanel, SessionRow (SessionMeta, not jobs)
│   │   ├── center/             # CenterPanel, Composer, Transcript (pi-format),
│   │   │                       #   views/ (Session | Task | Workflow | Empty)
│   │   ├── tasks/              # TasksPanel, TaskRow, TaskRowMenu, NewTaskModal
│   │   ├── workflows/          # WorkflowsPanel, WorkflowRow (read-only)
│   │   ├── projects/           # ProjectSwitcher (+ diagnostics), AddProjectModal
│   │   ├── agents/ · settings/ # AgentsModal, SettingsModal
│   │   └── shared/             # Menu (portal dropdown)
│   └── components/             # shared primitives: Modal, Markdown, NoticeBar, common
└── src-tauri/                  # Tauri (Rust) backend — a self-contained
    │                           # cargo crate (no root cargo workspace)
    ├── tauri.conf.json
    └── src/
        ├── lib.rs, main.rs     # command registry + setup(): frameless decorations (Windows)
        ├── commands.rs         # `daemon_request` chokepoint + settings/reconnect/status
        ├── state.rs            # AppState (connection, settings, connect-lock, epoch)
        ├── settings.rs         # persisted local/remote mode + host/token
        └── client/
            ├── rpc.rs          # persistent connection, response demux, notif re-emit
            ├── local.rs        # UDS connect + auto-spawn autoskd
            └── remote.rs       # TCP connect + token handshake
```

## Architecture

The design mirrors the CodexMonitor blueprint ("shared core + thin adapters"):

- **One invoke chokepoint.** `src/services/ipc.ts` is the only file that calls
  Tauri `invoke`. Every `autoskd` JSON-RPC method gets a typed shim there; the
  rest of the app calls those functions and never touches `invoke` directly.
  All shims forward through a single generic `daemon_request(method, params)`
  Tauri command.
- **One listen chokepoint.** `src/services/events.ts` is the only file that
  calls Tauri `listen`. It is a ref-counted fan-out hub: one Tauri listener per
  event name serves N React subscribers.
- **Transport-agnostic backend.** The Rust `daemon_request` command is the
  local-vs-remote switch (`if remote { tcp.call } else { uds.call }`).
  `autoskd` JSON-RPC notifications (`session-event`, `task-changed`,
  `project-changed`) are re-emitted verbatim via `app.emit("<same-name>",
  payload)`, so the React layer is oblivious to local-vs-remote. On connect the
  backend auto-issues the GLOBAL `project.subscribe` only; in proto v2
  `task.subscribe` requires `{cwd}` and OPENS that project, so it is
  per-active-project and front-end-issued (`state/store.tsx`), re-issued after a
  reconnect (it is per-connection state). Remote mode authenticates with
  `meta.auth{token}` before any other call.
- **State engine.** A normalized, slice-composed reducer keyed by
  project/task/session. A single `selection` (task | session | workflow |
  none) drives the polymorphic center; `state/store.tsx` holds the effects layer
  + the event router that maps notifications into reducer actions. The live
  transcript tail follows either the selected session or, when a task is
  selected, the task's newest running session (one `session.subscribe` at a
  time); the transcript is the pi-format `session.transcript` / `session-event`
  line stream, deduped by the transcript line cursor. `task-changed` carries the
  full `TaskView`, so the router upserts it directly instead of refetching.
- **UI shell.** A frameless window (macOS `titleBarStyle: Overlay`; Windows
  `decorations:false` + custom caption controls) over a two-panel layout: a
  left **sidebar** that stacks **Tasks**, **Sessions** (all agent runs, animated
  status dots) and **Workflows** as a lazygit-style accordion — the active panel
  grows (3:1 weight) while the others collapse, and selecting an entity
  auto-expands its matching panel — and a **main panel** with the polymorphic
  entity view (task sheet with editable/deletable comments / session transcript /
  **read-only** workflow definition) plus one entity-aware composer (steer a live
  session / comment a task). Workflows AND agents are code now (registered by
  project extensions), so both panels are read-only — there is no create / edit /
  install / uninstall UI. A titlebar hosts the connection indicator and the
  Agents/Settings modals; the titlebar project switcher (switch / add / init /
  remove) also surfaces a ⚠ **diagnostics** badge + list when an extension fails
  to load (`project.diagnostics`).

Both invariants are enforced by `scripts/check-ipc-discipline.mjs` (run as part
of `npm run typecheck`) and an eslint `no-restricted-imports` rule, so a stray
`invoke`/`listen` import fails the build.

### Connection lifecycle (local mode)

Mirrors the Go connector (`internal/daemon/rpcclient/connector.go`): resolve the
socket path (`$AUTOSK_SOCK` → `~/.autosk/daemon.sock`), connect-or-spawn
`autoskd serve --sock`, wait for readiness with bounded backoff. The `autoskd`
binary is located via `$AUTOSKD_BIN` → the app-exe / `Resources` dir → `PATH`.
Concurrent first-connects are single-flighted behind a connect lock, and each
connection is tagged with a generation epoch so a superseded connection's late
EOF cannot leak a stale `daemon-status: false` after the replacement is live.

## Scripts

```bash
cd gui
npm install

npm run dev          # Vite dev server (frontend only, no Tauri shell)
npm run typecheck    # tsc --noEmit + IPC-discipline guard
npm run test         # vitest (pure reducer/selectors/ipc logic; no browser/daemon)
npm run test:watch   # vitest watch mode
npm run build        # tsc && vite build (production bundle)
npm run lint         # eslint

npm run tauri:dev    # launch the desktop app (needs a display + webkit)
npm run tauri:build  # bundle the desktop app
```

Backend (Rust) checks run from `gui/src-tauri`:

```bash
cd gui/src-tauri
cargo check
cargo test
```

`gui/src-tauri` is a self-contained cargo crate (there is no root cargo
workspace — the daemon is Bun/TypeScript), so these commands are independent of
the rest of the repo.

## Local vs remote mode

Mode is an app setting (persisted; editable from the in-app **Settings** view):

- **Local** — connects over the UDS and auto-spawns `autoskd` if it is not
  already running. Behaves like `autosk lazy`: zero configuration when `autoskd`
  is reachable on `$PATH`.
- **Remote** — dials a configured `host:port` and authenticates with a token
  (first request is `meta.auth{token}`). The remote `autoskd` must be running
  explicitly (you cannot auto-spawn a process on another host). Behaviour is
  identical to local because the frontend is transport-agnostic.

## Known limitations / Phase-5 follow-ups

- **`autoskd` is not yet bundled as a Tauri sidecar.** `tauri.conf.json` has no
  `bundle.externalBin`, and nothing drops `autoskd` into the app bundle. Local
  mode relies on the runtime path resolver (`$AUTOSKD_BIN` → app-exe/`Resources`
  dir → `PATH`), so a packaged `.app` only works in local mode if `autoskd` is
  already on `PATH`. This is fine for the `npm run tauri:dev` milestone; wiring
  the real `externalBin` + per-target-triple sidecar drop is a **Phase 5**
  (release/CI) task.
- **Live runtime is built-to-contract, not headlessly exercised.**
  `npm run tauri:dev` needs a display and the full webkit bundle, so end-to-end
  behaviour (live streaming, composer round-trips, remote-mode parity) is built
  to the JSON-RPC contract and unit-covered at the pure-logic layer, but is not
  exercised in CI. Validate the display-dependent acceptance criteria in a GUI
  environment.
