# Feature documentation inventory

A working checklist of every user-facing feature in autosk, used to drive
iterative documentation work (one pass per feature via the `document-generate`
skill).

- Each leaf item is a candidate **documentation unit** (Diataxis: reference /
  how-to / tutorial / explanation).
- Granularity is **grouped** (~70 units): closely related verbs/actions share
  one unit.
- Mark progress per item: `[ ]` todo, `[~]` in progress, `[x]` documented.
- IDs are stable — reference them in tasks/commits (e.g. "docs: B5 list/ready").

> Compiled by a full code + docs sweep across the four surfaces (CLI, lazy TUI,
> desktop/mobile GUI) and the daemon/workflow/agent/extension/sandbox layers.

---

## A. Core concepts & data model

> Documented in **[docs/concepts.md](../concepts.md)** (cross-linked from
> `README.md`, `AGENTS.md`, `docs/daemon.md`, `docs/workflows.md`).

- [x] **A1.** Project overview — what autosk is (task tracker + workflow engine + three front ends)
- [x] **A2.** Task model — `id ask-XXXXXX`, title, description, workflow/step, timestamps
- [x] **A3.** Task status machine (`new` / `work` / `human` / `done` / `cancel`) and the *ready set*
- [x] **A4.** Blockers & dependency graph (`blocked_by` / `blocks`)
- [x] **A5.** Free-form `metadata` bag + reserved `step_visits` (visit caps, manual reset)
- [x] **A6.** Task comments as the cross-agent channel
- [x] **A7.** On-disk `.autosk/` layout (tasks/, sessions/, extensions/, settings.json); manual editing; hybrid file ownership

## B. CLI (`autosk`)

- [ ] **B1.** Global behavior — `--json`, `-q/--quiet`; daemon auto-spawn; read- vs write-clients
- [ ] **B2.** `init` + implicit auto-init (env: `AUTOSK_NO_AUTOINIT`, `AUTOSK_AUTOINIT_ASSUME_YES`)
- [ ] **B3.** Task lifecycle reads/edits — `create`, `show`, `list`/`ls`, `ready`, `next`, `update`
- [ ] **B4.** Status flips — `done`, `cancel`, `reopen`
- [ ] **B5.** Workflow ops — `enroll` (`--workflow`/`--step`), `resume` (`--to`)
- [ ] **B6.** Dependencies — `block`, `unblock` (`--all`), `dep list`
- [ ] **B7.** `comment` (add / list / edit / delete, `--author`)
- [ ] **B8.** `metadata` (show / set / unset)
- [ ] **B9.** `workflow` (list / show) — read-only
- [ ] **B10.** `session`/`sess` (list / get / transcript / abort / input `--followup`)
- [ ] **B11.** `project` (list / add / diagnostics)
- [ ] **B12.** `ext` (add / list / remove / update; `-l/--local`, `--global`, `--dry-run`/`--check`)
- [ ] **B13.** `version` (no auto-spawn)
- [ ] **B14.** CLI env vars — `AUTOSK_AGENT`, `AUTOSK_CWD`, `AUTOSK_SOCK`, `AUTOSKD_BIN`

## C. Lazy TUI (`autosk lazy`)

- [ ] **C1.** Overview & launch (flags `--sock`, `--refresh`, `--no-changelog`)
- [ ] **C2.** Panes — Tasks / Sessions / Workflows / Agents / Detail + accordion layout
- [ ] **C3.** Focus model, navigation & keybindings (per-pane bindings)
- [ ] **C4.** Task compose editor (`n`/`c`, two-pane)
- [ ] **C5.** Comment compose (`m`) and metadata `$EDITOR` editor (`M`)
- [ ] **C6.** Enroll/Resume two-pane picker (`e`/`r`)
- [ ] **C7.** Help cheatsheet (`?`)
- [ ] **C8.** Changelog modal (`ctrl+w` / first-run-of-release) + `~/.autosk/state.json`
- [ ] **C9.** Command palette (`:`)
- [ ] **C10.** Filtering & scope chips (`/`, `*`, `status:`/`wf:` facets)
- [ ] **C11.** Live session transcript streaming + markdown rendering + Tokyo Night theme
- [ ] **C12.** Session input (steer / followup / abort) textarea
- [ ] **C13.** Status bar / options strip / command log (`@`) / flash toasts / daemon health chip

## D. Desktop / Mobile GUI

- [ ] **D1.** Overview & layout (frameless workspace, sidebar accordion, resizer)
- [ ] **D2.** Tasks panel + task detail sheet
- [ ] **D3.** Sessions panel + session transcript (streaming, partials, rich blocks: text/thinking/tool)
- [ ] **D4.** Workflows panel + read-only workflow definition view
- [ ] **D5.** State-aware composer (chat / steer / comment / none)
- [ ] **D6.** Task actions (create / edit / done / cancel / reopen / block / unblock / enroll; comments)
- [ ] **D7.** Interactive chat sessions (new session, end)
- [ ] **D8.** Project switcher (add / init / remove / diagnostics badge)
- [ ] **D9.** Extension browser (npm search + install with scope choice)
- [ ] **D10.** Connection modes — Local (auto-spawn over UDS) vs Remote (host:port + token), Settings
- [ ] **D11.** Platform UX — desktop (macOS/Windows/Linux), iPad, iPhone compact single-pane
- [ ] **D12.** Settings (appearance/zoom, backend mode, remote daemon, connection status)
- [ ] **D13.** Distribution & install (Homebrew cask, AppImage/.deb, iOS TestFlight, `make install`)

## E. Daemon (`autoskd`)

> Documented in **[docs/daemon.md](../daemon.md)** (cross-linked from
> `README.md`, `AGENTS.md`, `docs/concepts.md`, `docs/workflows.md`,
> `docs/extensions.md`, `docs/gui-release.md`).

- [x] **E1.** Overview & architecture (one daemon per host, sole owner of `.autosk/`, pure-client front ends)
- [x] **E2.** Lifecycle — auto-spawn, single-instance lock, crash recovery, idle-shutdown
- [x] **E3.** `autoskd serve` flags (`--sock`, `--tcp`, `--workers`) + env (`AUTOSK_IDLE_SECS`, `AUTOSK_TOKEN_FILE`, `AUTOSK_NPM_BIN`, `AUTOSK_NO_AUTO_INSTALL`)
- [x] **E4.** Transports & auth (UDS perms, TCP token auth)
- [x] **E5.** JSON-RPC proto-v2 surface (meta / project / task / registry / extension / session + push notifications + error codes)
- [x] **E6.** Sessions & transcripts (pi-format, partial messages, steer/abort)
- [x] **E7.** `autoskd mcp` — standalone stdio MCP server
- [x] **E8.** Per-session host HTTP MCP server (`ctx.newMCPServer()`)
- [x] **E9.** MCP tool surface (`transit` / `task` / `comment`)

## F. Workflows (shipped)

- [ ] **F1.** Workflow concepts (`WorkflowDefinition`, `firstStep`, steps, `onTransit`, visit caps, `statusStep`)
- [ ] **F2.** `@autosk/feature-dev` — reference workflow, bootstrapped on first run
- [ ] **F3.** `@autosk/feature-dev-cc` — Claude Code variant
- [ ] **F4.** `@autosk/feature-dev-docker` — Docker-isolation variant
- [ ] **F5.** `@autosk/merge-to-current` — single-step merge of the task branch
- [ ] **F6.** Writing your own workflow

## G. Agents (shipped)

- [ ] **G1.** `AgentDefinition` contract + `AgentRunContext` (onRun/onSteer/onFollowup/onAbort)
- [ ] **G2.** `@autosk/pi-agent` (`pi --mode rpc`)
- [ ] **G3.** `@autosk/claude-agent` (Claude Code `claude -p`)
- [ ] **G4.** Interactive (taskless) chat sessions & named agents

## H. Sandboxes / isolation (`@autosk/sandbox`)

- [ ] **H1.** Sandbox concept (agent-owned isolation, structural `Sandbox` shape)
- [ ] **H2.** `worktreeSandbox()` — per-task git worktree
- [ ] **H3.** `dockerSandbox({ image })` — per-task container
- [ ] **H4.** `sandboxCleanupStep()` — teardown as a workflow step

## I. Extension system

> Documented in **[docs/extensions.md](../extensions.md)** (reference +
> explanation + management how-to) and the learn-by-doing
> **[docs/extensions-tutorial.md](../extensions-tutorial.md)** (write → run →
> break → recover). Cross-linked from `README.md` and `AGENTS.md`.

- [x] **I1.** Extension model (default-export factory + `AutoskAPI`: `registerWorkflow` / `registerAgent`)
- [x] **I2.** Discovery & precedence (project-local / global / settings npm, name-collision overrides)
- [x] **I3.** Provisioning — first-run bootstrap + auto-install reconcile + opt-out
- [x] **I4.** Error isolation & `project diagnostics`

---

## Doc-fix items (found during inventory)

- [ ] **FIX-1.** `@autosk/merge-to-current` is shipped + documented at the package level but **missing** from `docs/workflows.md` and `docs/extensions.md` — add it when documenting F5.

### Verified non-issues (no action)

- `autosk lazy` `j`/`k` scroll the task Detail pane (`internal/lazy/tui/keys.go:196-197`) — works as documented.
- `autosk_step` appears only in a historical design plan (`docs/plans/20260521-Short-Statuses.md`), not in any live doc/README. The current MCP tool surface is exactly `transit` / `task` / `comment` (no `autosk_step` tool).
