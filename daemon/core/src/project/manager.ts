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

import type { ProjectInfo } from "@autosk/sdk";

import { join } from "node:path";

import {
  ensureGlobalBootstrap,
  ensureExtensionsInstalled,
  loadProjectRegistry,
  validateInFlightTasks,
  type BootstrapOptions,
  type ExtensionEnv,
  type ExtensionRegistry,
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
      const home = this.extensionsEnv.home ?? process.env.HOME ?? "";
      if (!home) return;
      await ensureGlobalBootstrap({ home, logger: this.logger, ...bootstrap });
      // Reconcile the GLOBAL settings.json on every start: install any package
      // listed under `extensions` that is not yet present (e.g. an operator
      // hand-edited settings.json to add one). Only missing packages install.
      await ensureExtensionsInstalled({
        home,
        settingsPaths: [join(home, AUTOSK_DIR, "settings.json")],
        npmBin: bootstrap.npmBin,
        install: bootstrap.install,
        logger: this.logger,
      });
    })();
    return this.bootstrapOnce;
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
    const home = this.extensionsEnv.home ?? process.env.HOME ?? "";
    if (!home) return;
    const packagesDir = join(home, AUTOSK_DIR, "packages");
    await this.installLocks.run(packagesDir, () =>
      ensureExtensionsInstalled({
        home,
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
}
