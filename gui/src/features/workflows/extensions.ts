// Pure helpers for the extension browser: parsing the npm package name out of
// a `settings.json#extensions` entry, mapping installed entries to a name→scope
// lookup (so the browse modal can flag already-installed packages), and small
// presentational formatters. Kept side-effect-free so they unit-test without a
// daemon or the Tauri bridge (see extensions.test.ts).

import type { ExtensionEntryInfo } from "@/types";

/** The `npm:` source prefix used in `settings.json#extensions`. */
const NPM_PREFIX = "npm:";

/**
 * Strips a trailing `@version` from an npm spec, accounting for scoped names —
 * mirrors the daemon's `npmName` (daemon/core/src/extensions/source.ts):
 * `@scope/pkg@1.2.3` → `@scope/pkg`, `pkg@1.2.3` → `pkg`, `@scope/pkg` →
 * `@scope/pkg`. The result is the package NAME used as the npm identity.
 */
export function npmName(spec: string): string {
  if (spec.startsWith("@")) {
    const slash = spec.indexOf("/");
    if (slash < 0) return spec; // malformed scope; nothing to strip
    const at = spec.indexOf("@", slash);
    return at < 0 ? spec : spec.slice(0, at);
  }
  const at = spec.indexOf("@");
  return at <= 0 ? spec : spec.slice(0, at);
}

/**
 * Extracts the package name from an `extension.list` entry `source`. Only npm
 * entries (`npm:<spec>`) carry an identity we can match against a search row;
 * local-path / invalid entries return `null`.
 */
export function installedNameFromSource(source: string): string | null {
  const trimmed = source.trim();
  if (!trimmed.startsWith(NPM_PREFIX)) return null;
  const spec = trimmed.slice(NPM_PREFIX.length).trim();
  if (spec.length === 0) return null;
  return npmName(spec);
}

/** A package's installed scope, surfaced as the row badge. */
export type InstalledScope = "global" | "project";

/**
 * Builds a `packageName → scope` lookup from the `extension.list` entries. When
 * a package is installed in both scopes, "project" wins (it is the one that
 * shadows the global copy for the active project).
 */
export function installedScopes(entries: ExtensionEntryInfo[]): Map<string, InstalledScope> {
  const out = new Map<string, InstalledScope>();
  for (const e of entries) {
    const name = installedNameFromSource(e.source);
    if (!name) continue;
    if (e.scope === "project" || !out.has(name)) {
      out.set(name, e.scope);
    }
  }
  return out;
}

/** Compact weekly-downloads label: 1234 → "1,234". */
export function formatDownloads(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "0";
  return Math.round(n).toLocaleString();
}

// Date-only rendering of the `updated` timestamp reuses the shared
// `localDate` helper in components/common.tsx (no second null/NaN guard here).
