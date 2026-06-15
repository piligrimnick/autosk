/**
 * Explicit extension management — the engine behind `autosk ext add` /
 * `autosk ext list` / `autosk ext remove`.
 *
 * Unlike the first-run bootstrap and the per-start reconcile (which are gated by
 * `AUTOSK_NO_AUTO_INSTALL`), an EXPLICIT install always runs: the operator asked
 * for it. The scope (global `~/.autosk/` vs project `<root>/.autosk/`) is chosen
 * by the caller (`-l/--local`); this module only acts on the `packagesDir` +
 * `settingsPath` it is handed.
 *
 * Three operations:
 *  - {@link installExtension} — npm: `npm install <spec>` into `packagesDir`
 *    then upsert `settings.json`; local: verify the path resolves to a loadable
 *    extension then upsert (the path is loaded in place, never copied).
 *  - {@link removeExtensionFromSettings} — drop the same-identity entry from
 *    `settings.json` (node_modules is left untouched, like pi).
 *  - {@link listExtensionEntries} — classify the global + project settings
 *    entries with a `resolved` flag (does it actually load?).
 *
 * No hot-reload: a freshly-installed package is picked up on the next daemon
 * start / first project open (the registry is built once and cached). Callers
 * surface a restart hint.
 */

import { mkdirSync } from "node:fs";
import { join } from "node:path";

import { AUTOSK_DIR } from "../store/paths.ts";
import { consoleLogger, type Logger } from "../store/logger.ts";
import { ensurePackagesManifest, npmInstaller, type BootstrapInstaller } from "./bootstrap.ts";
import { resolveLocalPath, resolvePackageEntries } from "./discovery.ts";
import { readSettingsExtensions, removeExtensionEntry, upsertExtensionEntry } from "./settings.ts";
import { classifySettingsEntry, type ExtensionSource } from "./source.ts";

/** Options for {@link installExtension}. */
export interface InstallExtensionOptions {
  /** The parsed source (npm spec or absolute local path). */
  source: ExtensionSource;
  /** The packages prefix npm extensions install into (`<scope>/.autosk/packages`). */
  packagesDir: string;
  /** The `settings.json` the entry is upserted into. */
  settingsPath: string;
  /** `npm` binary (default `$AUTOSK_NPM_BIN` or `npm`). */
  npmBin?: string;
  /** Override the install step (tests). Default: shell out to `npm`. */
  install?: BootstrapInstaller;
  logger?: Logger;
}

/** Result of an explicit install. */
export interface InstallExtensionResult {
  /** Whether an npm install actually ran (false for a local-path source). */
  installed: boolean;
  /** The canonical `settings.json` entry written (`npm:<spec>` | `<abs-path>`). */
  entry: string;
}

/**
 * Installs one extension into a scope. npm: `npm install <spec>` into
 * `packagesDir` (throws on failure) then upsert `settings.json` (a re-install
 * with a different version replaces the pin — dedup by name). local: verify the
 * path resolves to a loadable extension (throws otherwise), then upsert (loaded
 * in place, never copied).
 */
export async function installExtension(opts: InstallExtensionOptions): Promise<InstallExtensionResult> {
  const logger = opts.logger ?? consoleLogger;
  if (opts.source.kind === "npm") {
    const install = opts.install ?? npmInstaller(opts.npmBin, logger);
    mkdirSync(opts.packagesDir, { recursive: true });
    ensurePackagesManifest(opts.packagesDir);
    const res = await install({ packagesDir: opts.packagesDir, packages: [opts.source.spec] });
    if (!res.ok) {
      throw new Error(`npm install ${opts.source.spec} failed: ${res.error ?? "unknown error"}`);
    }
    const { entry } = upsertExtensionEntry(opts.settingsPath, opts.source);
    return { installed: true, entry };
  }
  // local: the path is referenced in place (never copied) — resolve it now so a
  // typo or a non-extension path fails fast at install time, with the SAME
  // message the loader would emit, instead of being silently registered and
  // surfacing as `resolved: false` in `autosk ext list`.
  const resolution = resolveLocalPath(opts.source.path);
  if (resolution.entries.length === 0) {
    throw new Error(
      `local extension ${opts.source.path} is not loadable: ${resolution.error ?? "no extension entry found"}`,
    );
  }
  const { entry } = upsertExtensionEntry(opts.settingsPath, opts.source);
  return { installed: false, entry };
}

/** Options for {@link removeExtensionFromSettings}. */
export interface RemoveExtensionOptions {
  source: ExtensionSource;
  settingsPath: string;
}

/** Result of {@link removeExtensionFromSettings}. */
export interface RemoveExtensionResult {
  /** Whether at least one matching entry was removed. */
  removed: boolean;
  /** The actual entry strings dropped (may differ in version from the argument). */
  entries: string[];
}

/**
 * Removes a source's entry from `settings.json` (match by name for npm, by path
 * for local — any version). Does NOT prune node_modules (like pi). Idempotent.
 * Returns the entry strings actually removed so callers can report what was
 * dropped (an npm match by name may carry a different pinned version).
 */
export function removeExtensionFromSettings(opts: RemoveExtensionOptions): RemoveExtensionResult {
  const { removed } = removeExtensionEntry(opts.settingsPath, opts.source);
  return { removed: removed.length > 0, entries: removed };
}

/** One classified settings entry, surfaced by `autosk ext list`. */
export interface ExtensionEntryInfo {
  /** The raw `settings.json` entry (`npm:<spec>` | `<abs-path>` | junk). */
  source: string;
  /** Which settings file it came from. */
  scope: "global" | "project";
  /** Classification: an npm spec, a local path, or unrecognised. */
  kind: "npm" | "local" | "invalid";
  /** Whether it actually resolves to a loadable extension right now. */
  resolved: boolean;
}

/** Options for {@link listExtensionEntries}. */
export interface ListExtensionsOptions {
  /** The project root (its `<root>/.autosk/settings.json` is the project scope). Optional. */
  projectRoot?: string;
  /** Home dir (its `<home>/.autosk/settings.json` is the global scope). */
  home: string;
}

/**
 * Lists the global + project settings entries, classified, each with a
 * `resolved` flag. Global first, then project (a flat list; the loader's
 * project-beats-global precedence is separate). A missing settings file
 * contributes nothing.
 */
export function listExtensionEntries(opts: ListExtensionsOptions): ExtensionEntryInfo[] {
  const out: ExtensionEntryInfo[] = [];
  if (opts.home) {
    collectScope(out, "global", join(opts.home, AUTOSK_DIR), opts.home);
  }
  if (opts.projectRoot) {
    collectScope(out, "project", join(opts.projectRoot, AUTOSK_DIR), opts.home);
  }
  return out;
}

/** Reads one scope's settings, classifies each entry, and computes `resolved`. */
function collectScope(
  out: ExtensionEntryInfo[],
  scope: "global" | "project",
  autoskDir: string,
  home: string,
): void {
  const settingsPath = join(autoskDir, "settings.json");
  const packagesDir = join(autoskDir, "packages");
  for (const entry of readSettingsExtensions(settingsPath)) {
    const classified = classifySettingsEntry(entry, { baseDir: autoskDir, home });
    if (classified.kind === "invalid") {
      out.push({ source: entry, scope, kind: "invalid", resolved: false });
      continue;
    }
    if (classified.kind === "npm") {
      const resolved = resolvePackageEntries(packagesDir, classified.name).entries.length > 0;
      out.push({ source: entry, scope, kind: "npm", resolved });
    } else {
      const resolved = resolveLocalPath(classified.path).entries.length > 0;
      out.push({ source: entry, scope, kind: "local", resolved });
    }
  }
}
