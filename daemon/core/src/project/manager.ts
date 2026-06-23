/**
 * The per-daemon project cache (plan §3.7(1)).
 *
 * Each RPC carries a `{cwd}` selector (+ optional explicit path override). The
 * manager resolves it to a canonical root, opens that project lazily on first
 * sight (constructing its `Store`, running the startup scan, starting the
 * watcher), and caches the handle keyed by root so concurrent resolves of the
 * same project share one store. A per-root open lock serialises first-open so
 * two racing resolvers cannot both construct a store for one project.
 *
 * The handle bundles the file `Store` today; the extension registry (P3) and
 * scheduler (P4) get bundled here too.
 *
 * Resolving a project does NOT add it to the registry — only `addProject`
 * mutates `~/.autosk/projects.json`.
 */

import type {
  ExtensionInstallResult,
  ExtensionListResult,
  ExtensionRemoveResult,
  ExtensionUpdateResult,
  ProjectInfo,
} from "@autosk/sdk";

import { join } from "node:path";

import {
  ensureGlobalBootstrap,
  ensureExtensionsInstalled,
  InvalidExtensionSourceError,
  installExtension,
  listExtensionEntries,
  loadProjectRegistry,
  parseInstallSource,
  removeExtensionFromSettings,
  settingsEntryFor,
  updateExtensions,
  validateInFlightTasks,
  type BootstrapInstaller,
  type BootstrapOptions,
  type ExtensionEnv,
  type ExtensionRegistry,
  type ExtensionSource,
  type NpmViewVersion,
} from "../extensions/index.ts";
import { systemClock, type Clock } from "../store/clock.ts";
import { KeyedMutex } from "../store/lock.ts";
import { consoleLogger, type Logger } from "../store/logger.ts";
import { AUTOSK_DIR } from "../store/paths.ts";
import { Store, type StoreOptions } from "../store/store.ts";
import { initProject } from "./init.ts";
import { ProjectRegistry } from "./registry.ts";
import { canonicalize, resolveProjectRoot } from "./resolve.ts";

/**
 * An opened project: its canonical root, the bundled file store, and the
 * per-project extension registry (workflows + agents + load diagnostics).
 */
export interface ProjectHandle {
  root: string;
  store: Store;
  /** The project's extension registry (P3): workflows, agents, diagnostics. */
  extensions: ExtensionRegistry;
  /** RFC3339 UTC time the project was opened (for `healthz`). */
  opened_at: string;
}

export interface ProjectManagerOptions {
  /** The persisted registry (defaults to `~/.autosk/projects.json`). */
  registry?: ProjectRegistry;
  /** Options passed to every project's `Store`. */
  store?: StoreOptions;
  /** Clock for `opened_at` (defaults to the system clock). */
  clock?: Clock;
  /**
   * Extension loader environment (global-source `home`). Defaults to
   * `process.env.HOME`; tests inject a temp home so they never touch the real
   * `~/.autosk/`.
   */
  extensions?: ExtensionEnv;
  /** Logger for live-code hazard parks (defaults to the console logger). */
  logger?: Logger;
  /**
   * First-run bootstrap config (npm-install the default extensions into
   * `~/.autosk/packages/` + write `~/.autosk/settings.json` when it is absent).
   * `home`/`logger` are taken from the manager. Omit to DISABLE bootstrap (the
   * test default — tests must never trigger a real `npm install`); the
   * production daemon passes `{}` to enable it with the defaults.
   */
  bootstrap?: ProjectBootstrap;
}

/** Per-daemon bootstrap config (manager supplies `home`/`logger`). */
export type ProjectBootstrap = Omit<BootstrapOptions, "home" | "logger">;

export class ProjectManager {
  private readonly registry: ProjectRegistry;
  private readonly storeOpts: StoreOptions;
  private readonly clock: Clock;
  private readonly extensionsEnv: ExtensionEnv;
  private readonly logger: Logger;
  private readonly bootstrapConfig?: ProjectBootstrap;

  private projects = new Map<string, ProjectHandle>();
  private openLocks = new KeyedMutex();
  /**
   * Serialises `npm install` runs into the shared `~/.autosk/packages/` prefix
   * (keyed by that dir) so two projects opening concurrently never race a reconcile.
   */
  private installLocks = new KeyedMutex();
  /** Single-flight first-run bootstrap; resolved once per daemon process. */
  private bootstrapOnce: Promise<void> | null = null;

  constructor(opts: ProjectManagerOptions = {}) {
    this.registry = opts.registry ?? ProjectRegistry.openDefault();
    this.storeOpts = opts.store ?? {};
    this.clock = opts.clock ?? systemClock;
    this.extensionsEnv = opts.extensions ?? {};
    this.logger = opts.logger ?? consoleLogger;
    this.bootstrapConfig = opts.bootstrap;
  }

  /**
   * Runs the first-run environment bootstrap at most once (no-op when disabled,
   * when no home is resolvable, or once `settings.json` already exists). Awaited
   * before any project's extension registry is built, and kicked off eagerly at
   * daemon start so it is usually done by the time a project opens. Never throws.
   */
  ensureBootstrap(): Promise<void> {
    const bootstrap = this.bootstrapConfig;
    if (!bootstrap) return Promise.resolve();
    this.bootstrapOnce ??= (async () => {
      const home = this.homeDir();
      if (!home) return;
      await ensureGlobalBootstrap({ home, logger: this.logger, ...bootstrap });
      // Reconcile the GLOBAL settings.json on every start: install any npm
      // package listed under `extensions` that is not yet present (e.g. an
      // operator hand-edited settings.json to add one). Only missing install.
      await ensureExtensionsInstalled({
        packagesDir: join(home, AUTOSK_DIR, "packages"),
        settingsPaths: [join(home, AUTOSK_DIR, "settings.json")],
        npmBin: bootstrap.npmBin,
        install: bootstrap.install,
        logger: this.logger,
      });
    })();
    return this.bootstrapOnce;
  }

  /** The global home dir (`<home>/.autosk/…`); empty when unresolvable. */
  private homeDir(): string {
    return this.extensionsEnv.home ?? process.env.HOME ?? "";
  }

  /**
   * npm binary + installer/registry overrides for explicit installs and updates
   * (from the bootstrap config). The `view` seam lets tests inject a fake
   * registry so `extension.update` never hits the network.
   */
  private installerConfig(): { npmBin?: string; install?: BootstrapInstaller; view?: NpmViewVersion } {
    return {
      npmBin: this.bootstrapConfig?.npmBin,
      install: this.bootstrapConfig?.install,
      view: this.bootstrapConfig?.view,
    };
  }

  /**
   * Reconciles a project's `./.autosk/settings.json` packages on first open:
   * installs any listed-but-missing npm extension into the shared
   * `~/.autosk/packages/` prefix (the global settings are already reconciled by
   * {@link ensureBootstrap} at daemon start). Gated on bootstrap being enabled,
   * so tests with no `bootstrap` config never hit npm. Never throws.
   */
  private async reconcileProjectExtensions(root: string): Promise<void> {
    const bootstrap = this.bootstrapConfig;
    if (!bootstrap) return;
    // A project's listed npm extensions install into the PROJECT's own packages
    // dir (consistent with the loader, which resolves project-settings npm under
    // `<root>/.autosk/packages`). Serialised per packages dir.
    const packagesDir = join(root, AUTOSK_DIR, "packages");
    await this.installLocks.run(packagesDir, () =>
      ensureExtensionsInstalled({
        packagesDir,
        settingsPaths: [join(root, AUTOSK_DIR, "settings.json")],
        npmBin: bootstrap.npmBin,
        install: bootstrap.install,
        logger: this.logger,
      }),
    );
  }

  // -- resolution / lazy open ----------------------------------------------

  /** Resolves a `{cwd}` (+ optional override) to an opened project handle. */
  async resolve(cwd: string, override?: string): Promise<ProjectHandle> {
    const root = await resolveProjectRoot(cwd, override);
    return this.open(root);
  }

  /** Opens (or returns the cached) project handle for a canonical `root`. */
  async open(root: string): Promise<ProjectHandle> {
    const cached = this.projects.get(root);
    if (cached) return cached;
    // Provision the default extensions before the first registry build so a
    // fresh machine discovers `feature-dev` on its very first project open.
    await this.ensureBootstrap();
    return this.openLocks.run(root, async () => {
      const again = this.projects.get(root);
      if (again) return again;
      // Install any project-local settings.json extension that is listed but
      // not yet present, before the registry is built so it is discoverable.
      await this.reconcileProjectExtensions(root);
      const store = new Store(root, this.storeOpts);
      await store.open();
      try {
        // Build the per-project extension registry (discovery + factories, with
        // error isolation), then run the live-code hazard guard against the
        // just-loaded store: a `work` task whose workflow/step vanished from the
        // registry is parked to `human` before the scheduler can ever pick it up.
        const extensions = await loadProjectRegistry(root, this.extensionsEnv);
        // Surface load diagnostics in the daemon log too (not only via the
        // `project.diagnostics` RPC): a CLI-only operator who never calls that
        // RPC would otherwise have zero signal that an extension failed to load.
        const diags = extensions.diagnostics;
        if (diags.length > 0) {
          const sources = [...new Set(diags.map((d) => d.source))].join(", ");
          this.logger.warn(
            `extensions: ${diags.length} load diagnostic(s) opening ${root} ` +
              `(sources: ${sources}); see project.diagnostics`,
          );
        }
        const parked = await validateInFlightTasks(store, extensions);
        for (const p of parked) {
          this.logger.warn(`live-code hazard: parked ${p.taskId} to human (${p.error})`);
        }
        const handle: ProjectHandle = { root, store, extensions, opened_at: this.clock() };
        this.projects.set(root, handle);
        return handle;
      } catch (e) {
        // The registry load swallows its own errors, but a hazard-guard store
        // write could still fail; don't leave the store's watcher running for a
        // project that never got cached.
        await store.close();
        throw e;
      }
    });
  }

  /** Currently-loaded project handles (order unspecified). */
  loaded(): ProjectHandle[] {
    return [...this.projects.values()];
  }

  /** Closes every open store (stops watchers). */
  async close(): Promise<void> {
    for (const handle of this.projects.values()) {
      await handle.store.close();
    }
    this.projects.clear();
  }

  // -- registry (explicit list) --------------------------------------------

  /** Registered projects, ordered by root. */
  listProjects(): Promise<ProjectInfo[]> {
    return this.registry.list();
  }

  /**
   * Registers a project. Walks up from `cwd` to the nearest `.autosk/` (exactly
   * like {@link resolve} and every cwd-keyed read), so `project.add` works from
   * a nested subdirectory, not only from the project root. The canonical root is
   * stored. (Use {@link initProject} first for greenfield.)
   */
  async addProject(cwd: string, name?: string): Promise<ProjectInfo> {
    const root = await resolveProjectRoot(cwd);
    return this.registry.add(root, name);
  }

  /**
   * Unregisters the project resolved from `cwd`. Walks up like {@link addProject}
   * so removal works from a subdir; falls back to the canonical `cwd` when no
   * `.autosk/` is found, so a STALE registry entry can still be removed after
   * its project directory was deleted.
   */
  async removeProject(cwd: string): Promise<boolean> {
    let root: string;
    try {
      root = await resolveProjectRoot(cwd);
    } catch {
      root = await canonicalize(cwd);
    }
    return this.registry.remove(root);
  }

  /** Creates a `.autosk/` skeleton (does not register — see {@link addProject}). */
  initProject(dir: string): Promise<ProjectInfo> {
    return initProject(dir);
  }

  // -- extension management (autosk ext) -----------------------------------

  /**
   * Installs an extension into a scope. `local` → the project resolved from
   * `cwd` (its `<root>/.autosk/{packages,settings.json}`); else the global home
   * (`<home>/.autosk/{packages,settings.json}`). An explicit install always runs
   * — it is NOT gated by `AUTOSK_NO_AUTO_INSTALL`. No hot-reload: the new package
   * is picked up on the next daemon start / first project open.
   */
  async installExtension(cwd: string, source: string, local: boolean): Promise<ExtensionInstallResult> {
    const scopeDirs = await this.scopeDirs(cwd, local);
    const parsed = parseInstallSource(source, { cwd, home: this.homeDir() });
    const { npmBin, install } = this.installerConfig();
    const { installed, entry } = await this.installLocks.run(scopeDirs.packagesDir, () =>
      installExtension({
        source: parsed,
        packagesDir: scopeDirs.packagesDir,
        settingsPath: scopeDirs.settingsPath,
        npmBin,
        install,
        logger: this.logger,
      }),
    );
    return { scope: scopeDirs.scope, source: entry, settings_path: scopeDirs.settingsPath, installed };
  }

  /**
   * Removes an extension entry from a scope's `settings.json` (match by name for
   * npm, by path for local — any version). node_modules is left untouched.
   */
  async removeExtension(cwd: string, source: string, local: boolean): Promise<ExtensionRemoveResult> {
    const scopeDirs = await this.scopeDirs(cwd, local);
    const parsed = parseInstallSource(source, { cwd, home: this.homeDir() });
    // Serialise under the same per-scope lock as installExtension so concurrent
    // settings writers (install + remove, two removes) cannot lose an update on
    // the non-atomic read-modify-write of settings.json.
    const { removed, entries } = await this.installLocks.run(scopeDirs.packagesDir, async () =>
      removeExtensionFromSettings({ source: parsed, settingsPath: scopeDirs.settingsPath }),
    );
    // Report what was ACTUALLY removed (npm matches by name, so the stored entry
    // may pin a different version than the argument); when nothing matched, echo
    // the canonical entry form of the parsed source.
    const reported = entries[0] ?? settingsEntryFor(parsed);
    return { scope: scopeDirs.scope, source: reported, settings_path: scopeDirs.settingsPath, removed };
  }

  /**
   * Lists the global + project settings entries (classified, with a `resolved`
   * flag). Tolerant of a `cwd` outside any project — it then lists global only.
   */
  async listExtensions(cwd: string): Promise<ExtensionListResult> {
    let projectRoot: string | undefined;
    try {
      projectRoot = await resolveProjectRoot(cwd);
    } catch {
      projectRoot = undefined; // no project at cwd → global scope only
    }
    return { entries: listExtensionEntries({ projectRoot, home: this.homeDir() }) };
  }

  /**
   * Updates installed npm extensions to newer registry versions in place. Scope
   * selection mirrors how a project loads extensions:
   *  - `scope:"global"` → the global home only (no project required);
   *  - `scope:"project"` → the project resolved from `cwd` only (PROJECT_NOT_FOUND
   *    if none — same contract as `-l/--local`);
   *  - omitted ⇒ the UNION of global + project inside a project, or global only
   *    outside one.
   *
   * An optional `source` targets a single extension by npm name; a local-path
   * `source` is rejected (local entries load in place — nothing to update). Like
   * an explicit install, an update is NOT gated by `AUTOSK_NO_AUTO_INSTALL`, and
   * there is no hot-reload (callers surface a restart hint when anything changed).
   */
  async updateExtensions(
    cwd: string,
    opts: { source?: string; scope?: "global" | "project"; dryRun?: boolean },
  ): Promise<ExtensionUpdateResult> {
    // `scope:"project"` requires a project; otherwise resolve tolerantly so a
    // global/auto update works outside any project (auto → global only there).
    let projectRoot: string | undefined;
    if (opts.scope === "project") {
      projectRoot = await resolveProjectRoot(cwd);
    } else {
      try {
        projectRoot = await resolveProjectRoot(cwd);
      } catch {
        projectRoot = undefined;
      }
    }
    const home = this.homeDir();
    if (opts.scope !== "project" && !home) {
      throw new Error("cannot resolve home directory for a global extension update");
    }

    let source: ExtensionSource | undefined;
    if (opts.source) {
      const parsed = parseInstallSource(opts.source, { cwd, home });
      if (parsed.kind !== "npm") {
        throw new InvalidExtensionSourceError(
          `cannot update a local-path extension (${opts.source}); local extensions load in place — nothing to update`,
        );
      }
      source = parsed;
    }

    const { npmBin, install, view } = this.installerConfig();
    return updateExtensions({
      home,
      projectRoot,
      scopeFilter: opts.scope,
      source,
      dryRun: opts.dryRun ?? false,
      npmBin,
      install,
      view,
      // Serialise each scope's install under the same per-packages-dir lock as
      // install/remove so a concurrent add/remove/update can't corrupt the dir.
      runExclusive: (packagesDir, fn) => this.installLocks.run(packagesDir, fn),
      logger: this.logger,
    });
  }

  /**
   * Resolves the scope's packages dir + settings path. `local` requires a
   * project at `cwd` (resolveProjectRoot throws PROJECT_NOT_FOUND otherwise);
   * global uses `<home>/.autosk`.
   */
  private async scopeDirs(
    cwd: string,
    local: boolean,
  ): Promise<{ scope: "global" | "project"; packagesDir: string; settingsPath: string }> {
    if (local) {
      const root = await resolveProjectRoot(cwd);
      const autoskDir = join(root, AUTOSK_DIR);
      return { scope: "project", packagesDir: join(autoskDir, "packages"), settingsPath: join(autoskDir, "settings.json") };
    }
    const home = this.homeDir();
    if (!home) throw new Error("cannot resolve home directory for a global extension install");
    const autoskDir = join(home, AUTOSK_DIR);
    return { scope: "global", packagesDir: join(autoskDir, "packages"), settingsPath: join(autoskDir, "settings.json") };
  }
}
