# GUI extension browser — browse & install `autosk-extension` npm packages

Status: planned
Date: 2026-06-15

## Goal

Let an operator discover and install autosk extensions directly from the
desktop GUI. Add a `＋` action to the **Workflows** panel header (by analogy
with the Tasks panel). Clicking it opens a modal listing npm packages published
with the keyword `autosk-extension`, sorted by weekly download count
(descending). Clicking a package row opens its page on npmjs.com in the user's
browser. Each row has an **Install** button which opens a second modal —
"Where to install this extension?" — with two choices: **Globally** and
**To this project**.

## Decisions (locked)

- **Search** runs in the **Rust Tauri backend** (a new Tauri command); results
  are rendered in the GUI. **Install** reuses the existing daemon RPC
  `extension.install` (`{ cwd, source, local? }`). No new daemon RPC method.
- After a successful install: show a **"restart required" hint** (workflows are
  picked up on the next project open; no hot-reload — matches the CLI today).
- Open npm page via **`@tauri-apps/plugin-opener`** (the official Tauri v2
  plugin).
- Row fields: **name + version, description, weekly downloads, publisher, last
  updated date**.
- Already-installed packages: show an **"Installed" badge** (with scope) and
  **disable the Install button**; determined via the existing
  `extension.list` RPC.
- Modal shows the **fixed keyword list only** (no free-text search box), sorted
  by weekly downloads.
- **GUI only** — no CLI parity for the search this iteration.

## Architecture / flow

```
WorkflowsPanel [＋] ──▶ BrowseExtensionsModal
        │                 on open: invoke("extension_search")  ──▶ Rust ──▶ registry.npmjs.org
        │                          daemonRequest("extension.list", {cwd})  (Installed badges)
        │                 ▼
        │           ExtensionRow × N  (sorted by weekly downloads, desc)
        │             ├─ click row  ──▶ openExternal(npm_url)        [plugin-opener]
        │             └─ [Install]  ──▶ InstallScopeModal
        │                                 ├─ Globally    ──▶ extension.install {local:false}
        │                                 └─ To project  ──▶ extension.install {local:true}
        │                                         ▼
        └────────────────────────── success toast: "reopen project for workflows"
```

Notes:
- The npm search always executes on the machine running the GUI (Rust side),
  even in remote-daemon mode. Install goes to whichever daemon the GUI is
  connected to (local UDS or remote TCP) — consistent with the rest of the app.
- The `＋` button is only shown when a project is active (`cwd` present), exactly
  like the Tasks panel.

## Implementation

### 1. Rust backend — `extension_search` command

New file `gui/src-tauri/src/npm.rs`:
- `#[tauri::command] async fn extension_search() -> Result<Vec<NpmExtension>, String>`
- `GET https://registry.npmjs.org/-/v1/search?text=keywords:autosk-extension&size=250`
- Parse `objects[]`: `package.{name, version, description, date, publisher.username, links.npm}`,
  `downloads.weekly` (may be absent → 0), `updated`.
- Normalize into `NpmExtension { name, version, description, publisher,
  weekly_downloads, updated, npm_url }`.
- Sort by `weekly_downloads` desc on the Rust side.
- Network/parse errors → `Err(String)` (modal surfaces the message).

`gui/src-tauri/Cargo.toml`:
```toml
reqwest = { version = "0.12", default-features = false, features = ["json", "rustls-tls"] }
tauri-plugin-opener = "2"
```
(`rustls-tls` avoids an OpenSSL dependency; serde/serde_json already present via
tauri.)

`gui/src-tauri/src/lib.rs`:
- `.plugin(tauri_plugin_opener::init())`
- add `extension_search` to `generate_handler![...]`.

### 2. Capabilities — `gui/src-tauri/capabilities/default.json`

Add an opener permission **scoped to npmjs** (don't allow arbitrary URLs):
```json
{ "identifier": "opener:allow-open-url", "allow": [{ "url": "https://www.npmjs.com/*" }] }
```

### 3. IPC layer — `gui/src/services/ipc.ts` (the single invoke site)

The IPC-discipline guard (`scripts/check-ipc-discipline.mjs`) only flags
`invoke` imported from `@tauri-apps/api` **outside** `ipc.ts`. The file itself
may contain multiple `invoke` calls. Add shims here:
- `extensionSearch(): Promise<NpmExtension[]>` → `invoke("extension_search")`
  (a Tauri command — NOT via `daemonRequest`).
- `extensionList(cwd): Promise<ExtensionListResult>` →
  `daemonRequest("extension.list", { cwd })`.
- `extensionInstall(cwd, source, local): Promise<ExtensionInstallResult>` →
  `daemonRequest("extension.install", { cwd, source, local })`.

`openUrl` from `@tauri-apps/plugin-opener` is imported from a different module
path, so the guard does not flag it. For cleanliness wrap it in
`gui/src/services/opener.ts` (`openExternal(url)`).

### 4. Types — `gui/src/types.ts`

Add `NpmExtension`, `ExtensionEntryInfo`, `ExtensionListResult`,
`ExtensionInstallResult`. RPC mirror types stay snake_case on the wire; the
`extension_search` shape is defined to match the Rust struct.

### 5. Components — `gui/src/features/workflows/components/`

- `WorkflowsPanel.tsx` — next to the existing `↻` button add `＋`
  (title "Browse extensions"), a `useState` `browsing` flag, and at the end
  `{browsing && <BrowseExtensionsModal cwd={cwd} onClose={…} />}`. Gated on `cwd`.
- `BrowseExtensionsModal.tsx` (new) — on open, run `extensionSearch()` +
  `extensionList(cwd)` in parallel; loading / error / empty states; build a Set
  of installed names (with scope) by parsing each `source` (`npm:<name>@<ver>`
  → name is everything up to the LAST `@`, accounting for scoped `@scope/name`);
  render the `ExtensionRow` list.
- `ExtensionRow.tsx` (new) — name+version, description, weekly downloads,
  publisher, updated (formatted via the GUI's existing date formatter). Click on
  the row → `openExternal(npm_url)`. On the right: an **Install** button, or an
  **Installed (global/project)** badge + disabled button. The Install click must
  `stopPropagation` so it doesn't also open the npm page.
- `InstallScopeModal.tsx` (new) — title "Where to install this extension?", two
  buttons **Globally** (`local:false`) and **To this project** (`local:true`);
  calls `extensionInstall(cwd, name, local)`, busy/err state, on success closes
  both modals and shows the restart hint.

### 6. Post-install hint

Toast/message: `Installed <name> (<scope>). Reopen the project for its workflows
to appear.` Use the existing GUI toast mechanism if present, otherwise an inline
message in the modal. No auto-reload (per decision).

### 7. Styles

CSS for `.ext-list / .ext-row / .ext-badge` and alignment in the existing GUI
stylesheet.

### 8. Tests

- Rust: unit test for the parse+sort against a captured npm-search JSON fixture
  (no network).
- vitest: pure logic — the `source` → name parser, the installed-set mapping,
  sort/format helpers.
- `npm run typecheck` runs `check-ipc-discipline.mjs` — confirm `invoke` stays
  only in `ipc.ts`.
- `cargo check` for the Tauri backend.

### 9. Changelog

`CHANGELOG.md` → `## [Unreleased]` → `### Added`: "GUI: browse and install
`autosk-extension` npm packages from the Workflows panel."

### 10. Docs (optional)

Short note in `gui/README.md` and/or `docs/extensions.md`.

## Files touched

| Layer | File | Action |
|---|---|---|
| Rust | `gui/src-tauri/src/npm.rs` | new: command + structs |
| Rust | `gui/src-tauri/src/lib.rs` | register command + opener plugin |
| Rust | `gui/src-tauri/Cargo.toml` | `reqwest`, `tauri-plugin-opener` |
| Rust | `gui/src-tauri/capabilities/default.json` | opener permission (npmjs scope) |
| JS dep | `gui/package.json` | `@tauri-apps/plugin-opener` |
| TS | `gui/src/services/ipc.ts` | `extensionSearch/List/Install` shims |
| TS | `gui/src/services/opener.ts` | new: `openExternal` |
| TS | `gui/src/types.ts` | new types |
| TSX | `gui/src/features/workflows/components/WorkflowsPanel.tsx` | `＋` + modal |
| TSX | `BrowseExtensionsModal.tsx` / `ExtensionRow.tsx` / `InstallScopeModal.tsx` | new |
| CSS | existing GUI stylesheet | row/badge styles |
| Docs | `CHANGELOG.md` (+ opt. README/docs) | entry |

## Risks / nuances

- **npm search index lag**: a freshly published package may take minutes–hours
  to appear in the search index (direct `GET /<name>` is immediate). Mention in
  the empty/loading state.
- **Shipped packages** (`@autosk/feature-dev`, `@autosk/worktree`,
  `@autosk/pi-agent`) carry the `autosk-extension` keyword and will appear;
  feature-dev (installed globally by the first-run bootstrap) shows as
  **Installed (global)**. Expected.
- **Name parsing** from `source` must handle scoped names (`@scope/name@ver`).
- **No active project** → no `＋` button (matches Tasks); a global install uses
  the active project's `cwd` (irrelevant for npm specs).

## Acceptance criteria

1. The Workflows panel header shows a `＋` button (only when a project is active),
   alongside the existing `↻`.
2. Clicking `＋` opens a modal listing `autosk-extension` npm packages sorted by
   weekly downloads (descending), each showing name+version, description, weekly
   downloads, publisher, and last-updated date.
3. Clicking a package row opens its npmjs.com page in the user's default browser.
4. Each row has an **Install** button; already-installed packages instead show an
   **Installed (global/project)** badge with the Install button disabled.
5. Install opens a second modal "Where to install this extension?" with
   **Globally** and **To this project**; the choice maps to
   `extension.install { local: false | true }`.
6. After a successful install a hint is shown that the project must be reopened
   for the new workflow(s) to appear.
7. Loading, empty, and network-error states are handled in the browse modal.
8. `npm run typecheck` (incl. the IPC-discipline guard), `npm run test`,
   `npm run build`, and `cargo check` all pass; `invoke` remains confined to
   `src/services/ipc.ts`.
9. `CHANGELOG.md` has an `## [Unreleased] → ### Added` entry for the feature.
