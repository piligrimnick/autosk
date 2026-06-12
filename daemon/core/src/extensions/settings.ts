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

import { readFileSync } from "node:fs";

/**
 * Reads the `"extensions"` npm-package-name list from a `settings.json` file.
 * Returns `[]` for a missing/empty/corrupt file or a missing/non-array
 * `extensions` field. Only non-empty string entries survive.
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
