# `autosk ext` command group + `ext update`

Status: proposed · 2026-06-15

## Goal

1. **Regroup** the extension-management CLI under a single `ext` parent (clean
   break — the old top-level verbs are removed):
   - `autosk install <source>` → `autosk ext add <source>`
   - `autosk install list` → `autosk ext list`
   - `autosk install remove <source>` → `autosk ext remove <source>`
2. **Add** `autosk ext update [source]`: check the npm registry for newer
   versions of installed extensions and re-install them in place.
   - Outside an autosk project (project resolved by walking up): update the
     **global** scope only (`~/.autosk`).
   - Inside a project: update the **union** of global + project scopes
     (mirrors how a project actually loads extensions — project settings +
     global settings merged).

The daemon already owns all npm/`.autosk/` mechanics; the work is one new RPC
(`extension.update`) plus a CLI surface rename. No storage/loader changes.

---

## Decisions (locked)

| Topic | Decision |
| --- | --- |
| Old `install*` verbs | **Removed entirely** (clean break — no aliases). `autosk install` becomes an unknown command. |
| In-project update scope | **Union** of global + project. |
| Targeting | Optional `[source]` positional to update one extension + scope-override flags `-l/--local` (project-only) and `--global` (global-only). |
| Pinned / local | **Skip** version-pinned npm specs (`npm:foo@1.2.3`) and **skip** local-path entries (loaded in place — nothing to update). Only floating npm entries (`npm:foo`) are bumped. |
| UX | `--dry-run`/`--check` (report available updates, install nothing) + show **before → after** versions. Inherits the persistent `--json` / `--quiet` flags for parity with sibling verbs. |
| Offline env | **No** offline short-circuit (the user asked for none). Explicit `ext update` is operator-requested, so — like explicit `ext add` — it is **not** gated by `AUTOSK_NO_AUTO_INSTALL`. |
| Version-check mechanism | `npm view <name> version --json` vs the installed `node_modules/<name>/package.json` version (mirror pi). |
| Comparison | Plain inequality (`latest !== installed` ⇒ update). Mirrors pi; documented caveat: a registry `latest` lower than installed would "update" downward. (Could be tightened to semver-newer later.) |
| Version-check failure | **Fail-open** (mirror pi): treat as "needs update" in a real run; surfaced as `unknown` in `--dry-run`. |

---

## Background (current state)

### CLI
- `cmd/autosk/install.go` defines the whole group: `newInstallCmd()` (parent
  `install <source>`) + children `newInstallListCmd()` / `newInstallRemoveCmd()`.
- Registered at `cmd/autosk/main.go:44` (`newInstallCmd()`).
- `--json` / `--quiet` are **persistent root flags** (`main.go:14-15,27-28`),
  so every subcommand already honours them.
- Tests: `cmd/autosk/install_test.go` (local-path lifecycle + bare-source
  rejection) against an auto-spawned daemon.

### Wire / client (proto-v2)
- Methods `extension.install` / `extension.list` / `extension.remove`
  (`daemon/sdk/src/proto.ts:227-348`, `RPC_METHODS` list).
- Go client: `internal/daemon/rpcclient/writes.go:170-198`
  (`InstallExtension` / `RemoveExtension` / `ListExtensions`).
- Go view types: `internal/daemon/api/types.go:188-224`.
- Dispatch: `daemon/core/src/rpc/daemon.ts:517-529`.

### Daemon engine
- `ProjectManager` (`daemon/core/src/project/manager.ts`):
  `installExtension` / `removeExtension` / `listExtensions` (290-350);
  `scopeDirs(cwd, local)` (357-369) → `{scope, packagesDir, settingsPath}`;
  `homeDir()` (147-150); `installerConfig()` (152-154) returns `{npmBin, install}`
  from `bootstrapConfig`.
- Core: `daemon/core/src/extensions/install.ts`
  (`installExtension` / `listExtensionEntries` / `collectScope`),
  `bootstrap.ts` (`npmInstaller` shells `npm install <pkgs> --no-audit --no-fund`
  in `packagesDir`; discards stdout), `source.ts`
  (`npmName(spec)`, `classifySettingsEntry`).
- Storage: global `~/.autosk/{packages,settings.json}`, project
  `<root>/.autosk/{packages,settings.json}`. `settings.json#extensions` is the
  manifest; npm packages live under `<scope>/.autosk/packages/node_modules/<name>`.
- **Pinned detection is free**: a classified npm entry is pinned iff
  `spec !== name` (`npmName` strips a trailing `@version`).
- **No hot-reload**: the registry is built once per project open and cached, so
  any change (install or update) needs a daemon restart / project reopen —
  hence the existing restart hint, which `update` must also print when it
  actually changes node_modules.

---

## Design — `extension.update`

### New core module: `daemon/core/src/extensions/update.ts`

```ts
export interface UpdateExtensionsOptions {
  home: string;
  projectRoot?: string;                 // present ⇒ inside a project
  scopeFilter?: "global" | "project";   // absent ⇒ auto (union in project, else global)
  source?: ExtensionSource;             // optional single-target (match npm by name)
  dryRun?: boolean;
  npmBin?: string;
  install?: BootstrapInstaller;         // injected in tests
  view?: NpmViewVersion;                // injected in tests (registry lookup)
  logger?: Logger;
}

export interface UpdateEntryResult {
  source: string;                       // raw settings entry (npm:<spec>)
  name: string;                         // npm package name
  scope: "global" | "project";
  status: "updated" | "up-to-date" | "skipped" | "failed" | "available" | "unknown";
  from_version?: string;                // installed before
  to_version?: string;                  // latest (or installed-after)
  reason?: string;                      // skip/fail explanation
}

export interface UpdateExtensionsResult {
  entries: UpdateEntryResult[];
  dry_run: boolean;
  changed: boolean;                     // any status === "updated" (drives restart hint)
}
```

Algorithm (mirrors pi's `update()` / `updateConfiguredSources()`):

1. **Enumerate candidates** by reusing the `collectScope` walk
   (`install.ts`): for each in-scope `settings.json#extensions` entry, classify
   it. Keep only `kind === "npm"` that are **not pinned** (`spec === name`).
   - `scopeFilter` chooses which scopes to walk: `global` → home only;
     `project` → project root only (error if `projectRoot` absent);
     auto → home + project (project root only if present).
   - `source` filter: keep entries whose `name` matches `npmName(source.spec)`;
     if a single `source` matches nothing, throw `INVALID_PARAMS` with a
     "did you mean npm:<name>?" hint (mirrors pi's `buildNoMatchingPackageMessage`).
   - Non-npm / pinned / local entries that match an explicit `source` are
     reported as `skipped` with a reason (so a targeted update isn't silently a
     no-op); in a bulk run they're simply omitted.
2. **Version check** (concurrency limit 4, like pi's `UPDATE_CHECK_CONCURRENCY`):
   - installed = `node_modules/<name>/package.json` `version` (undefined ⇒
     "not installed" ⇒ needs update).
   - latest = `npm view <name> version --json` (new `npmViewVersion(npmBin)`
     helper in `bootstrap.ts`, a capturing sibling of `npmInstaller`, 10s
     timeout).
   - `latest !== installed` ⇒ candidate for update. View failure ⇒ fail-open
     (real run: still update; dry-run: `unknown`).
3. **Dry run**: emit `available` (with from/to) / `up-to-date` / `unknown`,
   install nothing.
4. **Apply** (real run): per scope, batch a single
   `install({packagesDir, packages: names.map(n => `${n}@latest`)})` into that
   scope's `packagesDir` (reuses the existing `npmInstaller` and per-scope
   `installLocks`). Re-read each package's version for the `to_version`.
   Floating settings entries (`npm:foo`) need no rewrite — only node_modules
   moves. Per-scope failure marks that scope's entries `failed` (others still
   run).

> The `install` dir **is** the update mechanism: `npm install <name>@latest`
> into `<scope>/.autosk/packages` is identical to how pi re-installs into its
> managed npm root.

### ProjectManager method

```ts
async updateExtensions(cwd: string, opts: {
  source?: string; scope?: "global" | "project"; dryRun?: boolean;
}): Promise<ExtensionUpdateResult>
```
- Resolve `projectRoot` tolerantly (`resolveProjectRoot(cwd)` in try/catch →
  `undefined`), exactly like `listExtensions`.
- `scope === "project"` requires a project (throw `PROJECT_NOT_FOUND` if none —
  same contract as `-l/--local` on add/remove).
- Parse `source` via `parseInstallSource` when present (reject local-path
  targets with a clear message — local isn't updatable).
- Run under `installLocks.run(packagesDir, …)` per scope (reuse the existing
  per-scope lock so a concurrent add/remove/update can't corrupt the
  read-modify-write).
- Pass `installerConfig()` (`{npmBin, install}`) plus a new `view` from
  `bootstrapConfig` (see below).

### Test seam

Extend `bootstrapConfig` (and `installerConfig()`) with an optional
`view?: NpmViewVersion` override so daemon unit tests inject fake registry
versions and never hit the network — same pattern as the existing `install`
override.

### Proto-v2 wire types (`daemon/sdk/src/proto.ts`)

```ts
export interface ExtensionUpdateParams extends ProjectSelector {
  source?: string;
  scope?: "global" | "project";   // absent = auto
  dry_run?: boolean;
}
export interface ExtensionUpdateEntry {
  source: string; name: string; scope: "global" | "project";
  status: "updated" | "up-to-date" | "skipped" | "failed" | "available" | "unknown";
  from_version?: string; to_version?: string; reason?: string;
}
export interface ExtensionUpdateResult { entries: ExtensionUpdateEntry[]; dry_run: boolean; changed: boolean; }
```
Add `"extension.update": { params: ExtensionUpdateParams; result: ExtensionUpdateResult }`
to `RpcMethodMap` and to the runtime `RPC_METHODS` array.

### Dispatch (`daemon/core/src/rpc/daemon.ts`)

```ts
"extension.update": async (params) => {
  const o = asObj(params);
  return this.projectManager.updateExtensions(reqCwd(o), {
    source: optString(o, "source"),
    scope: optString(o, "scope") as "global" | "project" | undefined,
    dryRun: optBool(o, "dry_run") ?? false,
  });
},
```

### Go client + view types

- `internal/daemon/api/types.go`: add `ExtensionUpdateEntry` /
  `ExtensionUpdateResult` mirroring the proto (RFC3339-free; no timeformat —
  these are wire types).
- `internal/daemon/rpcclient/writes.go`: add
  `UpdateExtensions(ctx, source string, scope string, dryRun bool)` building the
  selector (`source`/`scope`/`dry_run` only when set) and calling
  `extension.update`.

---

## CLI surface (`cmd/autosk/ext.go`, renamed from `install.go`)

Replace `newInstallCmd()` with `newExtCmd()`:

```
autosk ext add <source>      [-l|--local]      # was: autosk install
autosk ext list                                # was: autosk install list
autosk ext remove <source>   [-l|--local]      # was: autosk install remove
autosk ext update [source]   [-l|--local | --global] [--dry-run|--check]
```

- `ext` parent: `RunE` unset (prints help when called bare), or default to
  `ext list`. Help text documents the source grammar (`npm:<spec>` / path).
- `add` / `list` / `remove`: bodies are the current install/list/remove `RunE`
  verbatim (same client calls, same `--json`/`--quiet` handling, same restart
  hint).
- `update`:
  - flags: `-l/--local` (bool), `--global` (bool, mutually exclusive with
    `-l`), `--dry-run` (bool, alias `--check`).
  - maps flags → wire `scope`: `--local` ⇒ `project`, `--global` ⇒ `global`,
    neither ⇒ omit (daemon auto-detects).
  - calls `cl.UpdateExtensions(ctx, source, scope, dryRun)`.
  - `--json` ⇒ encode the result; otherwise a tabwriter table:
    `SCOPE  PACKAGE  FROM  TO  STATUS`, then a summary line
    (`updated N · up-to-date M · skipped K · failed J`); print the daemon
    restart hint to stderr when `res.Changed` and not dry-run; exit non-zero if
    any entry is `failed`.
  - empty candidate set ⇒ `(no updatable extensions)` to stderr.
- `cmd/autosk/main.go:44`: `newInstallCmd()` → `newExtCmd()`.

---

## Files touched

**Daemon (TS):**
- `daemon/core/src/extensions/update.ts` *(new)* — core update engine.
- `daemon/core/src/extensions/bootstrap.ts` — add `npmViewVersion(npmBin)`
  capturing helper + `NpmViewVersion` type.
- `daemon/core/src/project/manager.ts` — `updateExtensions(...)`; extend
  `installerConfig()`/`bootstrapConfig` with `view`.
- `daemon/core/src/rpc/daemon.ts` — `extension.update` handler.
- `daemon/sdk/src/proto.ts` — params/result types + `RpcMethodMap` + `RPC_METHODS`.

**Go:**
- `internal/daemon/api/types.go` — `ExtensionUpdate*` view types.
- `internal/daemon/rpcclient/writes.go` — `UpdateExtensions(...)`.
- `cmd/autosk/install.go` → **rename** `cmd/autosk/ext.go` — `newExtCmd()` group
  with `add`/`list`/`remove`/`update`.
- `cmd/autosk/main.go` — register `newExtCmd()`.

**Tests:**
- `cmd/autosk/install_test.go` → `cmd/autosk/ext_test.go` — retarget
  `runRoot(... "install" ...)` to `"ext","add"` etc.; add an `ext update
  --dry-run --json` test over a local-path extension (asserts local is skipped,
  no network).
- `daemon/core/test/extensions.update.test.ts` *(new)* — inject fake
  `install` + `view`: floating-npm updated when latest ≠ installed; up-to-date
  skip; pinned skip; local skip; per-scope batching; union vs `--global`/`-l`
  scope selection; single-`source` filter + "did you mean"; dry-run installs
  nothing; fail-open on view error.
- `daemon/core/test/rpc.conformance.test.ts` — round-trip `extension.update`.
- `daemon/sdk/test/proto.test.ts` — assert `extension.update` ∈ `RPC_METHODS`.

**Docs / changelog:**
- `CHANGELOG.md` (`## [Unreleased]`):
  - `### Changed` — extension management moved under `autosk ext`
    (`ext add`/`list`/`remove`).
  - `### Added` — `autosk ext update [source]` with `-l`/`--global`/`--dry-run`.
  - `### Removed` — top-level `autosk install` / `install list` / `install remove`.
- `docs/extensions.md` — rewrite the install-verb section for `ext add/list/
  remove`, document `ext update` (scope rules, pinned/local skip, dry-run).
- `AGENTS.md` — update the `autosk install` reference.
- Doc-comment accuracy (non-load-bearing): `ext.go` header, `install.ts`
  header, `manager.ts` "(autosk install)" comments, `daemon.ts` comment,
  `proto.ts` comment, `types.go` comment.
- Grep `internal/lazy/` + `gui/` for any `autosk install` references (TUI
  help / first-run popup, GUI strings) and update; the new RPC needs no GUI
  change (additive).

---

## Open questions / risks

- **Comparison semantics**: plan mirrors pi's plain `!==`. If a "never
  downgrade" guarantee is wanted, swap to a semver-newer check (a small
  comparator) — easy follow-up, not blocking.
- **`ext` parent default action**: print help vs. alias to `ext list`. Proposed:
  print help (Cobra default) to avoid surprising network-free listing on a bare
  `ext`.
- **Targeted update of a pinned/local source**: reported as `skipped` with a
  reason rather than silently doing nothing — confirm that UX.
- **`update` exit code**: non-zero when any package failed; `0` when everything
  was up-to-date or updated. Dry-run always `0`.

---

## Validation

- `cd daemon && bun run typecheck && bun test`
- `make build-autoskd && make test` (Go verb tests auto-spawn the rebuilt daemon)
- `make vet && make lint`
- Manual: `autosk ext list`, `autosk ext update --dry-run`, `autosk ext update`
  (global, outside a project) and inside a project (union); `autosk install`
  errors as unknown command.
