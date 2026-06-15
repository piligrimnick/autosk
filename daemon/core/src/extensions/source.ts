/**
 * Extension source parsing + classification (the `autosk install` model).
 *
 * autosk follows pi's explicit-source convention: a source is EITHER an
 * `npm:`-prefixed package spec (optionally `@version`) OR a local path
 * (`/abs`, `./rel`, `../rel`, `~/path`). There is deliberately NO implicit
 * bare-name → npm form — a bare token (`my-ext`) is rejected on install and
 * recorded as a diagnostic when it appears in `settings.json#extensions`.
 *
 * Two entry points:
 *  - {@link parseInstallSource} parses a CLI/RPC `source` argument (relative
 *    paths resolve against the caller's `cwd`); it THROWS on an unrecognised
 *    source so `autosk install foo` fails loudly.
 *  - {@link classifySettingsEntry} classifies a `settings.json#extensions`
 *    string (relative paths resolve against the settings file's directory); it
 *    returns an `{ kind: "invalid" }` discriminant for an unrecognised entry so
 *    the loader records a diagnostic instead of crashing.
 *
 * Identity (for dedup / upsert / remove): npm sources match by NAME (the
 * version is ignored), local sources match by absolute PATH.
 */

import { isAbsolute, resolve } from "node:path";

/** The `npm:` source prefix. */
const NPM_PREFIX = "npm:";

/** A parsed, recognised extension source. */
export type ExtensionSource =
  | { kind: "npm"; spec: string; name: string }
  | { kind: "local"; path: string };

/** The result of classifying a `settings.json` entry: a source or an explanation. */
export type ClassifiedEntry = ExtensionSource | { kind: "invalid"; reason: string };

/** Resolution context: `cwd` for relative install args, `home` for `~` expansion. */
export interface SourceContext {
  cwd: string;
  home: string;
}

/** Settings-classification context: `baseDir` for relative entries, `home` for `~`. */
export interface SettingsContext {
  baseDir: string;
  home: string;
}

/** Thrown by {@link parseInstallSource} for an unrecognised source. */
export class InvalidExtensionSourceError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "InvalidExtensionSourceError";
  }
}

/**
 * Strips a trailing `@version` from an npm spec, accounting for scoped names
 * (`@scope/pkg@1.2.3` → `@scope/pkg`, `pkg@1.2.3` → `pkg`, `@scope/pkg` →
 * `@scope/pkg`). The result is the package NAME used as the npm identity.
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
 * Whether `raw` looks like a local path source (`/abs`, `./rel`, `../rel`,
 * `~`, `~/path`, or any absolute path). A bare token is NOT a path.
 */
export function isLocalPathSpec(raw: string): boolean {
  return (
    raw === "~" ||
    raw.startsWith("~/") ||
    raw.startsWith("./") ||
    raw.startsWith("../") ||
    raw.startsWith("/") ||
    isAbsolute(raw)
  );
}

/** Expands a leading `~` against `home`, then resolves against `baseDir`. */
function resolveLocal(raw: string, baseDir: string, home: string): string {
  let p = raw;
  if (p === "~") p = home;
  else if (p.startsWith("~/")) p = `${home}/${p.slice(2)}`;
  return isAbsolute(p) ? resolve(p) : resolve(baseDir, p);
}

/**
 * Parses a CLI/RPC install `source`. `npm:<spec>` → an npm source; a local path
 * (`/abs`, `./rel`, `../rel`, `~/path`) → an absolute local source resolved
 * against `cwd`. Anything else THROWS {@link InvalidExtensionSourceError} — there
 * is no implicit bare-name → npm form.
 */
export function parseInstallSource(raw: string, ctx: SourceContext): ExtensionSource {
  const trimmed = raw.trim();
  if (trimmed.length === 0) {
    throw new InvalidExtensionSourceError("extension source must not be empty");
  }
  if (trimmed.startsWith(NPM_PREFIX)) {
    const spec = trimmed.slice(NPM_PREFIX.length).trim();
    if (spec.length === 0) {
      throw new InvalidExtensionSourceError(`invalid npm source ${JSON.stringify(raw)}: missing package spec`);
    }
    return { kind: "npm", spec, name: npmName(spec) };
  }
  if (isLocalPathSpec(trimmed)) {
    return { kind: "local", path: resolveLocal(trimmed, ctx.cwd, ctx.home) };
  }
  throw new InvalidExtensionSourceError(
    `unrecognised extension source ${JSON.stringify(raw)}: use "npm:<spec>" or a path (/abs, ./rel, ../rel, ~/path)`,
  );
}

/**
 * Classifies a `settings.json#extensions` entry. `npm:<spec>` → an npm source;
 * an absolute or relative path → a local source resolved against `baseDir`.
 * Anything else → `{ kind: "invalid" }` (the loader records a diagnostic).
 */
export function classifySettingsEntry(entry: string, ctx: SettingsContext): ClassifiedEntry {
  const trimmed = entry.trim();
  if (trimmed.length === 0) {
    return { kind: "invalid", reason: "empty extension entry" };
  }
  if (trimmed.startsWith(NPM_PREFIX)) {
    const spec = trimmed.slice(NPM_PREFIX.length).trim();
    if (spec.length === 0) return { kind: "invalid", reason: "npm: entry has no package spec" };
    return { kind: "npm", spec, name: npmName(spec) };
  }
  if (isLocalPathSpec(trimmed)) {
    return { kind: "local", path: resolveLocal(trimmed, ctx.baseDir, ctx.home) };
  }
  return {
    kind: "invalid",
    reason: `unrecognised extension entry (use "npm:<spec>" or an absolute path; no implicit bare-name → npm)`,
  };
}

/** The `settings.json#extensions` string for a source (npm:<spec> | <abs-path>). */
export function settingsEntryFor(source: ExtensionSource): string {
  return source.kind === "npm" ? `${NPM_PREFIX}${source.spec}` : source.path;
}

/** Whether two sources share an identity (npm: same name; local: same path). */
export function sameSource(a: ExtensionSource, b: ExtensionSource): boolean {
  if (a.kind === "npm" && b.kind === "npm") return a.name === b.name;
  if (a.kind === "local" && b.kind === "local") return a.path === b.path;
  return false;
}
