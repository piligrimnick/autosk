/**
 * `settings.json` reading for the npm-package extension source (plan §3.6,
 * step 1c).
 *
 * The global `~/.autosk/settings.json` and the project `./.autosk/settings.json`
 * may list npm package names under `"extensions"`; those packages are installed
 * into `~/.autosk/packages/` (`npm --prefix …`, like v1's agent packages) and
 * resolved to entry points by `discovery.ts`.
 *
 * A missing or unparseable settings file reads as "no packages" — a corrupt
 * settings.json must never brick project open; it just contributes nothing.
 */

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";

import {
  classifySettingsEntry,
  sameSource,
  settingsEntryFor,
  type ExtensionSource,
} from "./source.ts";

/**
 * Reads the `"extensions"` list from a `settings.json` file. Returns `[]` for a
 * missing/empty/corrupt file or a missing/non-array `extensions` field. Only
 * non-empty string entries survive (entries may be `npm:<spec>` or a path; the
 * loader/classifier interprets them).
 */
export function readSettingsExtensions(settingsPath: string): string[] {
  let text: string;
  try {
    text = readFileSync(settingsPath, "utf8");
  } catch {
    return [];
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return [];
  }
  const exts = (parsed as { extensions?: unknown })?.extensions;
  if (!Array.isArray(exts)) return [];
  return exts.filter((e): e is string => typeof e === "string" && e.length > 0);
}

/** Serialises `extensions` back to disk as `{"extensions":[...]}` + trailing newline. */
function writeSettingsExtensions(settingsPath: string, extensions: string[]): void {
  mkdirSync(dirname(settingsPath), { recursive: true });
  writeFileSync(settingsPath, `${JSON.stringify({ extensions }, null, 2)}\n`, "utf8");
}

/**
 * Whether a settings entry shares `source`'s identity (npm: same name; local:
 * same absolute path). An entry that does not classify never matches.
 */
function entryMatchesSource(entry: string, source: ExtensionSource, baseDir: string, home: string): boolean {
  const classified = classifySettingsEntry(entry, { baseDir, home });
  if (classified.kind === "invalid") return false;
  return sameSource(classified, source);
}

/**
 * Upserts `source` into `settings.json#extensions`: removes any same-identity
 * entry (so a re-install with a different npm version REPLACES the old pin),
 * appends the canonical entry string, and rewrites the file (creating the parent
 * dir). Returns the written `entry` and whether the on-disk list changed.
 */
export function upsertExtensionEntry(
  settingsPath: string,
  source: ExtensionSource,
): { entry: string; changed: boolean } {
  const baseDir = dirname(settingsPath);
  const home = process.env.HOME ?? "";
  const entry = settingsEntryFor(source);
  const existing = readSettingsExtensions(settingsPath);
  const kept = existing.filter((e) => !entryMatchesSource(e, source, baseDir, home));
  const next = [...kept, entry];
  const changed = existing.length !== next.length || existing.some((e, i) => e !== next[i]);
  if (changed) writeSettingsExtensions(settingsPath, next);
  return { entry, changed };
}

/**
 * Removes every same-identity entry for `source` from `settings.json#extensions`
 * (match by name for npm, by path for local — any version). Returns the removed
 * entry strings; the file is rewritten only when something was removed (a
 * missing file is a no-op). node_modules is left untouched (like pi).
 */
export function removeExtensionEntry(settingsPath: string, source: ExtensionSource): { removed: string[] } {
  const baseDir = dirname(settingsPath);
  const home = process.env.HOME ?? "";
  const existing = readSettingsExtensions(settingsPath);
  const removed: string[] = [];
  const kept = existing.filter((e) => {
    if (entryMatchesSource(e, source, baseDir, home)) {
      removed.push(e);
      return false;
    }
    return true;
  });
  if (removed.length > 0) writeSettingsExtensions(settingsPath, kept);
  return { removed };
}
