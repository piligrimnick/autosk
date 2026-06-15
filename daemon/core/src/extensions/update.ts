/**
 * `autosk ext update` engine — bump installed npm extensions to newer registry
 * versions in place.
 *
 * Like the explicit `autosk ext add`, an update is operator-requested, so it is
 * NOT gated by `AUTOSK_NO_AUTO_INSTALL`. It mirrors pi's `update()`:
 *
 *  1. ENUMERATE — walk the in-scope `settings.json#extensions` (reusing the
 *     classifier). Only FLOATING npm entries (`npm:foo`, where `spec === name`)
 *     are update CANDIDATES. Version-pinned npm (`npm:foo@1.2.3`) and local-path
 *     entries are reported as `skipped` (nothing to update — pins are explicit,
 *     local paths load in place); unrecognised entries are ignored (diagnosed
 *     elsewhere).
 *  2. VERSION-CHECK (concurrency-limited) — installed =
 *     `node_modules/<name>/package.json` `version`; latest =
 *     `npm view <name> version --json`. `latest !== installed` ⇒ a candidate to
 *     bump. A registry lookup failure is FAIL-OPEN (real run: update anyway;
 *     dry-run: surfaced as `unknown`).
 *  3. APPLY — per scope, batch a single `npm install <name>@latest …` into that
 *     scope's `packages` dir (the same mechanism `ext add` uses). Floating
 *     settings entries need no rewrite — only `node_modules` moves.
 *
 * No hot-reload: a bumped package is picked up on the next daemon start / first
 * project open, so callers surface a restart hint when anything changed.
 */

import { mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import type { ExtensionUpdateEntry, ExtensionUpdateResult } from "@autosk/sdk";

import { AUTOSK_DIR } from "../store/paths.ts";
import { consoleLogger, type Logger } from "../store/logger.ts";
import {
  ensurePackagesManifest,
  npmInstaller,
  npmViewVersion,
  type BootstrapInstaller,
  type NpmViewVersion,
} from "./bootstrap.ts";
import { readSettingsExtensions } from "./settings.ts";
import { classifySettingsEntry, InvalidExtensionSourceError, type ExtensionSource } from "./source.ts";

/** Options for {@link updateExtensions}. */
export interface UpdateExtensionsOptions {
  /** Global home dir (`<home>/.autosk/…`). */
  home: string;
  /** The project root, when the caller is inside a project (its `<root>/.autosk/…`). */
  projectRoot?: string;
  /**
   * Which scope(s) to update. `global` → home only; `project` → project only
   * (requires `projectRoot`); absent ⇒ auto: the UNION of global + project
   * inside a project, or global only outside one.
   */
  scopeFilter?: "global" | "project";
  /** Optional single-target filter (an npm source; matched by package name). */
  source?: ExtensionSource;
  /** Report available updates without installing anything. */
  dryRun?: boolean;
  /** `npm` binary (default `$AUTOSK_NPM_BIN` or `npm`). */
  npmBin?: string;
  /** Override the install step (tests). Default: shell out to `npm`. */
  install?: BootstrapInstaller;
  /** Override the registry version lookup (tests). Default: shell out to `npm view`. */
  view?: NpmViewVersion;
  /** Serialise each scope's npm install (per packages dir). Default: run inline. */
  runExclusive?: (packagesDir: string, fn: () => Promise<void>) => Promise<void>;
  /** Concurrency for the registry version checks (default 4). */
  concurrency?: number;
  logger?: Logger;
}

/** One scope to walk for candidate extensions. */
interface ScopeDir {
  scope: "global" | "project";
  autoskDir: string;
}

/** A floating npm entry that will be version-checked / possibly bumped. */
interface Candidate {
  source: string;
  name: string;
  scope: "global" | "project";
  packagesDir: string;
}

/** A version-checked candidate. */
interface Checked extends Candidate {
  installed?: string;
  latest?: string;
  viewFailed: boolean;
}

/**
 * Updates the in-scope floating npm extensions. See the module header for the
 * three phases. Throws {@link InvalidExtensionSourceError} (⇒ INVALID_PARAMS)
 * when a single `source` matches nothing installed.
 */
export async function updateExtensions(opts: UpdateExtensionsOptions): Promise<ExtensionUpdateResult> {
  const logger = opts.logger ?? consoleLogger;
  const dryRun = opts.dryRun ?? false;
  const view = opts.view ?? npmViewVersion(opts.npmBin, logger);
  const install = opts.install ?? npmInstaller(opts.npmBin, logger);
  const runExclusive = opts.runExclusive ?? ((_dir, fn) => fn());
  const targetName = opts.source?.kind === "npm" ? opts.source.name : undefined;

  // -- 1. enumerate candidates + skip notes --------------------------------
  const scopes = scopesToWalk(opts);
  const entries: ExtensionUpdateEntry[] = [];
  const candidates: Candidate[] = [];
  let targetMatched = false;

  for (const { scope, autoskDir } of scopes) {
    const settingsPath = join(autoskDir, "settings.json");
    const packagesDir = join(autoskDir, "packages");
    for (const raw of readSettingsExtensions(settingsPath)) {
      const classified = classifySettingsEntry(raw, { baseDir: autoskDir, home: opts.home });
      if (classified.kind === "invalid") continue; // diagnosed via project.diagnostics
      if (classified.kind === "local") {
        // A local entry can never match an npm-name target; in a bulk run it is
        // surfaced as skipped so the operator sees it was considered.
        if (targetName !== undefined) continue;
        entries.push({
          source: raw,
          name: classified.path,
          scope,
          status: "skipped",
          reason: "local path — loaded in place, nothing to update",
        });
        continue;
      }
      // npm
      const name = classified.name;
      if (targetName !== undefined && name !== targetName) continue;
      targetMatched = true;
      const pinned = classified.spec !== name;
      if (pinned) {
        entries.push({
          source: raw,
          name,
          scope,
          status: "skipped",
          reason: `version-pinned (${raw}) — not updated`,
        });
        continue;
      }
      candidates.push({ source: raw, name, scope, packagesDir });
    }
  }

  // A targeted update that matched no installed entry is a hard error (mirrors
  // pi's "no matching package" message) so a typo isn't a silent no-op.
  if (targetName !== undefined && !targetMatched) {
    throw new InvalidExtensionSourceError(
      `no installed extension named ${targetName}; did you mean \`autosk ext add npm:${targetName}\`?`,
    );
  }

  // -- 2. version-check the floating candidates (concurrency-limited) ------
  const checked = await mapWithConcurrency(candidates, opts.concurrency ?? 4, async (c): Promise<Checked> => {
    const installed = readInstalledVersion(c.packagesDir, c.name);
    const res = await view(c.name);
    if (!res.ok || !res.version) {
      return { ...c, installed, latest: undefined, viewFailed: true };
    }
    return { ...c, installed, latest: res.version, viewFailed: false };
  });

  // -- 3a. dry-run: report only, install nothing ---------------------------
  if (dryRun) {
    for (const c of checked) {
      if (c.viewFailed) {
        entries.push({ source: c.source, name: c.name, scope: c.scope, status: "unknown", from_version: c.installed });
      } else if (c.latest === c.installed) {
        entries.push({
          source: c.source,
          name: c.name,
          scope: c.scope,
          status: "up-to-date",
          from_version: c.installed,
          to_version: c.latest,
        });
      } else {
        entries.push({
          source: c.source,
          name: c.name,
          scope: c.scope,
          status: "available",
          from_version: c.installed,
          to_version: c.latest,
        });
      }
    }
    return { entries, dry_run: true, changed: false };
  }

  // -- 3b. real run: up-to-date pass-through, batch-install the rest -------
  const needUpdate: Checked[] = [];
  for (const c of checked) {
    const stale = c.viewFailed || c.latest !== c.installed;
    if (!stale) {
      entries.push({
        source: c.source,
        name: c.name,
        scope: c.scope,
        status: "up-to-date",
        from_version: c.installed,
        to_version: c.latest,
      });
      continue;
    }
    needUpdate.push(c);
  }

  // Group by scope's packages dir so each scope is a single batched install.
  const byDir = new Map<string, { packagesDir: string; items: Checked[] }>();
  for (const c of needUpdate) {
    const g = byDir.get(c.packagesDir) ?? { packagesDir: c.packagesDir, items: [] };
    g.items.push(c);
    byDir.set(c.packagesDir, g);
  }

  for (const { packagesDir, items } of byDir.values()) {
    const names = items.map((c) => c.name);
    logger.info(`autoskd: updating extension package(s): ${names.join(", ")} into ${packagesDir}`);
    await runExclusive(packagesDir, async () => {
      let result: { ok: boolean; error?: string };
      try {
        mkdirSync(packagesDir, { recursive: true });
        ensurePackagesManifest(packagesDir);
        result = await install({ packagesDir, packages: names.map((n) => `${n}@latest`) });
      } catch (e) {
        result = { ok: false, error: e instanceof Error ? e.message : String(e) };
      }
      if (!result.ok) {
        const reason = result.error ?? "npm install failed";
        for (const c of items) {
          entries.push({
            source: c.source,
            name: c.name,
            scope: c.scope,
            status: "failed",
            from_version: c.installed,
            reason,
          });
        }
        return;
      }
      for (const c of items) {
        const after = readInstalledVersion(packagesDir, c.name);
        entries.push({
          source: c.source,
          name: c.name,
          scope: c.scope,
          status: "updated",
          from_version: c.installed,
          to_version: after ?? c.latest,
        });
      }
    });
  }

  const changed = entries.some((e) => e.status === "updated");
  return { entries, dry_run: false, changed };
}

/** Which scope dirs to walk, honouring `scopeFilter` (see {@link UpdateExtensionsOptions}). */
function scopesToWalk(opts: UpdateExtensionsOptions): ScopeDir[] {
  const out: ScopeDir[] = [];
  const wantGlobal = opts.scopeFilter !== "project";
  const wantProject = opts.scopeFilter !== "global";
  if (wantGlobal && opts.home) out.push({ scope: "global", autoskDir: join(opts.home, AUTOSK_DIR) });
  if (wantProject && opts.projectRoot) {
    out.push({ scope: "project", autoskDir: join(opts.projectRoot, AUTOSK_DIR) });
  }
  return out;
}

/** Reads `<packagesDir>/node_modules/<name>/package.json` `version` (undefined if absent/corrupt). */
function readInstalledVersion(packagesDir: string, name: string): string | undefined {
  try {
    const pkg = JSON.parse(readFileSync(join(packagesDir, "node_modules", name, "package.json"), "utf8"));
    return pkg !== null && typeof pkg === "object" && typeof (pkg as { version?: unknown }).version === "string"
      ? (pkg as { version: string }).version
      : undefined;
  } catch {
    return undefined;
  }
}

/** Maps `fn` over `items` with at most `limit` concurrent in flight, preserving order. */
async function mapWithConcurrency<T, R>(items: T[], limit: number, fn: (item: T) => Promise<R>): Promise<R[]> {
  if (items.length === 0) return [];
  const results = new Array<R>(items.length);
  let next = 0;
  const worker = async (): Promise<void> => {
    for (;;) {
      const idx = next++;
      if (idx >= items.length) return;
      results[idx] = await fn(items[idx]!);
    }
  };
  await Promise.all(Array.from({ length: Math.min(Math.max(1, limit), items.length) }, worker));
  return results;
}
