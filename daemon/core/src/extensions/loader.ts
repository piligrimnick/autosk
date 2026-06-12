/**
 * The extension loader (plan §3.6, steps 1–3).
 *
 * Discovers extension entry points across the three sources (project-local dir,
 * global dir, npm packages from `settings.json`), merges them in **priority
 * order** (project beats global beats npm), Bun-imports each entry module, and
 * invokes its default-export factory with a per-extension {@link AutoskAPI}
 * handle that registers into the project's {@link ExtensionRegistry}.
 *
 * **No trust model** (plan §3.6): a discovered/installed extension is loaded,
 * full stop — there is no prompt-on-first-load gate anywhere in this path.
 *
 * **Error isolation** (pi's `ExtensionRunner.onError` model): a failed import,
 * a missing default export, a throwing factory, and a name collision are each
 * caught, recorded as a load diagnostic, and never crash the daemon or block
 * the remaining extensions.
 */

import { join } from "node:path";
import { pathToFileURL } from "node:url";

import type { AutoskAPI, ExtensionFactory, ExtensionLoadError } from "@autosk/sdk";

import { AUTOSK_DIR } from "../store/paths.ts";
import { discoverDir, resolvePackageEntries, type ExtensionEntry } from "./discovery.ts";
import { ExtensionRegistry } from "./registry.ts";
import { readSettingsExtensions } from "./settings.ts";

/** Loader environment — where the global sources live. */
export interface ExtensionEnv {
  /**
   * Home directory for the global sources (`<home>/.autosk/extensions`,
   * `<home>/.autosk/settings.json`, `<home>/.autosk/packages`). Defaults to
   * `process.env.HOME`. Injected by tests so they never touch the real `$HOME`.
   */
  home?: string;
}

/**
 * The merged discovery result for a project: the priority-ordered, deduplicated
 * entry list plus any diagnostics raised during discovery itself (today: a
 * `settings.json`-listed npm package that resolved to nothing). Discovery-time
 * diagnostics are kept separate from load-time ones so {@link loadProjectRegistry}
 * can fold them into the same `project.diagnostics` surface.
 */
export interface ResolvedProjectEntries {
  entries: ExtensionEntry[];
  /** Settings-listed packages that contributed no entries (not installed / empty). */
  packageDiagnostics: ExtensionLoadError[];
}

/**
 * Resolves the merged, deduplicated, priority-ordered list of extension entries
 * for a project (plan §3.6, step 1), plus discovery-time package diagnostics.
 *
 * Order (highest priority first — first occurrence of a path wins on dedup, and
 * first-registered wins on a name collision, so this ordering IS the priority):
 *   1. project-local dir   `<root>/.autosk/extensions/`
 *   2. global dir          `<home>/.autosk/extensions/`
 *   3. npm packages from `settings.json#extensions`, project settings before
 *      global settings, resolved under `<home>/.autosk/packages/node_modules/`.
 */
export function resolveProjectEntries(
  projectRoot: string,
  env: ExtensionEnv = {},
): ResolvedProjectEntries {
  const home = env.home ?? process.env.HOME ?? "";

  const projectExtDir = join(projectRoot, AUTOSK_DIR, "extensions");
  const projectSettings = join(projectRoot, AUTOSK_DIR, "settings.json");
  const globalExtDir = home ? join(home, AUTOSK_DIR, "extensions") : "";
  const globalSettings = home ? join(home, AUTOSK_DIR, "settings.json") : "";
  const packagesDir = home ? join(home, AUTOSK_DIR, "packages") : "";

  const ordered: ExtensionEntry[] = [];

  // (1) project-local dir, then (2) global dir.
  ordered.push(...discoverDir(projectExtDir));
  if (globalExtDir) ordered.push(...discoverDir(globalExtDir));

  // (3) npm packages: project settings first, then global; dedup by name.
  const pkgNames: string[] = [];
  const seenPkg = new Set<string>();
  const settingsLists = [readSettingsExtensions(projectSettings)];
  if (globalSettings) settingsLists.push(readSettingsExtensions(globalSettings));
  for (const list of settingsLists) {
    for (const name of list) {
      if (seenPkg.has(name)) continue;
      seenPkg.add(name);
      pkgNames.push(name);
    }
  }
  // An operator that LISTS a package but never installs it (typo, forgot to
  // install) gets a diagnostic rather than silence — this is the one source with
  // no load-time error surface, so we give it a discovery-time one.
  const packageDiagnostics: ExtensionLoadError[] = [];
  if (packagesDir) {
    for (const name of pkgNames) {
      const res = resolvePackageEntries(packagesDir, name);
      ordered.push(...res.entries);
      if (res.error) packageDiagnostics.push({ source: name, error: res.error });
    }
  }

  // Dedup by entry path — first occurrence (highest priority) wins.
  const seen = new Set<string>();
  const entries: ExtensionEntry[] = [];
  for (const entry of ordered) {
    if (seen.has(entry.entryPath)) continue;
    seen.add(entry.entryPath);
    entries.push(entry);
  }
  return { entries, packageDiagnostics };
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/**
 * Imports one entry module and runs its factory into `registry`. Every failure
 * mode is caught + recorded as a diagnostic — this never throws. Bun imports
 * `.ts`/`.js` natively, so no jiti/transpile step is needed.
 *
 * **Reload semantics (plan §3.6: "the registry at daemon start is the truth").**
 * A project's registry is built once, when the project is first opened, and is
 * then cached on the project handle for the daemon's lifetime. Bun caches every
 * imported module by its resolved specifier for the life of the process, and
 * there is no reliable in-process ESM cache invalidation (a query-string
 * cache-bust is silently ignored for `file://` URLs, and copying the entry to a
 * unique path would break its relative / `@autosk/sdk` imports). So editing an
 * extension's code is reflected only after a **daemon restart** — a fresh
 * process rebuilds every registry from the current on-disk code. The live-code
 * hazard guard (`hazard.ts`) handles the consequence: a restart whose new code
 * dropped a workflow/step parks the now-orphaned in-flight tasks to `human`.
 */
async function loadEntry(registry: ExtensionRegistry, entry: ExtensionEntry): Promise<void> {
  let mod: unknown;
  try {
    mod = await import(pathToFileURL(entry.entryPath).href);
  } catch (e) {
    registry.recordDiagnostic(entry.source, `failed to import: ${errMsg(e)}`);
    return;
  }

  const factory = (mod as { default?: unknown }).default;
  if (typeof factory !== "function") {
    registry.recordDiagnostic(entry.source, "extension has no default-export factory function");
    return;
  }

  const api: AutoskAPI = {
    registerWorkflow: (workflow) => registry.addWorkflow(entry.source, workflow),
    registerAgent: (agent) => registry.addAgent(entry.source, agent),
  };
  try {
    await (factory as ExtensionFactory)(api);
  } catch (e) {
    registry.recordDiagnostic(entry.source, `factory threw: ${errMsg(e)}`);
  }
}

/**
 * Builds a fresh {@link ExtensionRegistry} for a project: discover → import →
 * invoke factories, in priority order, with full error isolation. The returned
 * registry carries both the registered workflows/agents and the load
 * diagnostics (for `project.diagnostics`).
 */
export async function loadProjectRegistry(
  projectRoot: string,
  env: ExtensionEnv = {},
): Promise<ExtensionRegistry> {
  const registry = new ExtensionRegistry();
  const { entries, packageDiagnostics } = resolveProjectEntries(projectRoot, env);
  // Record discovery-time diagnostics (e.g. a listed-but-not-installed package)
  // before the factories run, so they sit at the top of `project.diagnostics`.
  for (const d of packageDiagnostics) registry.recordDiagnostic(d.source, d.error);
  // Sequential (not Promise.all): registration order is the priority order, so
  // first-registered wins a name collision deterministically.
  for (const entry of entries) {
    await loadEntry(registry, entry);
  }
  return registry;
}
