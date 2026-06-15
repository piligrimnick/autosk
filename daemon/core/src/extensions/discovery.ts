/**
 * pi-style extension discovery (plan §3.6, step 1).
 *
 * Mirrors pi's `discoverExtensionsInDir` / `resolveExtensionEntries` (see
 * `pi/packages/coding-agent/src/core/extensions/loader.ts`) so an autosk
 * extension directory follows the exact shapes pi operators already know.
 *
 * Per-directory discovery rules (one level deep, no recursion):
 *  1. Direct files — `*.ts` / `*.js` → an entry.
 *  2. Subdir with an index — `<sub>/index.ts` or `<sub>/index.js` → an entry.
 *  3. Subdir as a package — `<sub>/package.json#autosk.extensions` → the
 *     declared entry paths (relative to the subdir).
 *
 * Directory entries are read in **sorted** order so the load/merge order is
 * deterministic (acceptance: "load order/merge is deterministic and asserted").
 */

import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { join, resolve } from "node:path";

/**
 * A discovered extension entry: the absolute module path to import, plus an
 * operator-facing `source` label for diagnostics — a file/dir path for the
 * directory sources, an npm package name for the settings source.
 */
export interface ExtensionEntry {
  /** Absolute path to the entry module to import. */
  entryPath: string;
  /** Operator-facing label for `project.diagnostics` (path or package name). */
  source: string;
}

/** Whether a filename is an importable extension module (`.ts` / `.js`). */
export function isExtensionFile(name: string): boolean {
  return name.endsWith(".ts") || name.endsWith(".js");
}

/**
 * Reads `package.json#autosk.extensions` → its array of declared (string)
 * entry paths, or `null` when the file is absent/unparseable or has no such
 * field. The paths are returned verbatim (resolved against the package dir by
 * {@link resolveExtensionEntries}).
 */
export function readAutoskExtensionPaths(packageJsonPath: string): string[] | null {
  let pkg: unknown;
  try {
    pkg = JSON.parse(readFileSync(packageJsonPath, "utf8"));
  } catch {
    return null;
  }
  // A syntactically valid but non-object package.json (the JSON literal `null`,
  // a number, a string, an array) must not crash discovery — guard before the
  // property access so a malformed manifest only contributes nothing, it never
  // takes the whole project registry down (error-isolation invariant, §3.6).
  if (pkg === null || typeof pkg !== "object") return null;
  const autosk = (pkg as { autosk?: unknown }).autosk;
  if (!autosk || typeof autosk !== "object") return null;
  const exts = (autosk as { extensions?: unknown }).extensions;
  if (!Array.isArray(exts)) return null;
  return exts.filter((e): e is string => typeof e === "string" && e.length > 0);
}

/**
 * Resolves a package/subdir's entry points (rule 2 + 3): a
 * `package.json#autosk.extensions` manifest wins (its existing declared paths),
 * else `index.ts` / `index.js`. Returns the absolute entry paths, or `null`
 * when the directory declares no extension.
 */
export function resolveExtensionEntries(dir: string): string[] | null {
  const pkgJson = join(dir, "package.json");
  if (existsSync(pkgJson)) {
    const declared = readAutoskExtensionPaths(pkgJson);
    if (declared && declared.length > 0) {
      const entries: string[] = [];
      for (const rel of declared) {
        const abs = resolve(dir, rel);
        if (existsSync(abs)) entries.push(abs);
      }
      if (entries.length > 0) return entries;
    }
  }
  for (const index of ["index.ts", "index.js"]) {
    const p = join(dir, index);
    if (existsSync(p)) return [p];
  }
  return null;
}

/**
 * Discovers all extension entries directly under `dir` (one level), in sorted
 * filename order. A non-existent or unreadable directory yields `[]` (the
 * global/project dirs are optional). The `source` label of every entry is the
 * file/subdir path it was discovered from.
 */
export function discoverDir(dir: string): ExtensionEntry[] {
  if (!existsSync(dir)) return [];
  let names: string[];
  try {
    names = readdirSync(dir).sort();
  } catch {
    return [];
  }
  const entries: ExtensionEntry[] = [];
  for (const name of names) {
    if (name.startsWith(".")) continue;
    const entryPath = join(dir, name);
    let isFile = false;
    let isDir = false;
    try {
      // `statSync` follows symlinks, so a symlinked file/dir is handled like
      // its target (pi accepts symlinked extensions the same way).
      const st = statSync(entryPath);
      isFile = st.isFile();
      isDir = st.isDirectory();
    } catch {
      continue;
    }
    if (isFile && isExtensionFile(name)) {
      entries.push({ entryPath, source: entryPath });
      continue;
    }
    if (isDir) {
      const resolved = resolveExtensionEntries(entryPath);
      if (resolved) {
        for (const e of resolved) entries.push({ entryPath: e, source: entryPath });
      }
    }
  }
  return entries;
}

/**
 * The outcome of resolving one `settings.json#extensions` package: its entry
 * points (possibly empty) plus, when it contributed nothing, an operator-facing
 * `error` explaining why. The error is what gives the npm source a diagnostic
 * surface (the dir/file sources surface import/parse failures at load time, but
 * a never-installed package would otherwise fail completely silently — the
 * operator listed it, so they deserve feedback that it never loaded).
 */
export interface PackageResolution {
  entries: ExtensionEntry[];
  /** Present iff the package resolved to no entries; the reason it was skipped. */
  error?: string;
}

/**
 * Resolves an npm package (listed in `settings.json#extensions`) installed
 * under `<packagesDir>/node_modules/<name>` to its declared entry points
 * (rule 3 / index fallback). The `source` label of every entry is the package
 * NAME (operator-facing), not the on-disk path.
 *
 * A package that is **not installed** or that is installed but **declares no
 * extension** yields no entries AND an `error` (distinct messages), so the
 * loader can record a `project.diagnostics` entry reflecting the operator's
 * stated intent instead of silently dropping the name.
 */
export function resolvePackageEntries(packagesDir: string, packageName: string): PackageResolution {
  const nodeModules = join(packagesDir, "node_modules");
  const packageDir = join(nodeModules, packageName);
  if (!existsSync(packageDir)) {
    return { entries: [], error: `not installed under ${nodeModules} (run \`autosk install npm:${packageName}\`?)` };
  }
  const resolved = resolveExtensionEntries(packageDir);
  if (!resolved) {
    return {
      entries: [],
      error: "installed but declares no extension (no package.json#autosk.extensions entry or index.ts/js)",
    };
  }
  return { entries: resolved.map((entryPath) => ({ entryPath, source: packageName })) };
}

/**
 * Resolves a LOCAL extension source (a `settings.json` absolute path that points
 * at a file or directory in place — pi-style, never copied) to its entry points.
 * A `.ts`/`.js` file is itself the entry; a directory resolves via
 * {@link resolveExtensionEntries} (package.json#autosk.extensions / index). The
 * `source` label of every entry is the absolute path.
 *
 * A path that does not exist, a non-extension file, or a directory declaring no
 * extension yields no entries AND an `error`, so the loader records a
 * `project.diagnostics` entry reflecting the operator's stated intent.
 */
export function resolveLocalPath(absPath: string): PackageResolution {
  let st;
  try {
    st = statSync(absPath);
  } catch {
    return { entries: [], error: `local path not found: ${absPath}` };
  }
  if (st.isFile()) {
    if (!isExtensionFile(absPath)) {
      return { entries: [], error: `local path is not a .ts/.js extension file: ${absPath}` };
    }
    return { entries: [{ entryPath: absPath, source: absPath }] };
  }
  if (st.isDirectory()) {
    const resolved = resolveExtensionEntries(absPath);
    if (!resolved) {
      return {
        entries: [],
        error: "directory declares no extension (no package.json#autosk.extensions entry or index.ts/js)",
      };
    }
    return { entries: resolved.map((entryPath) => ({ entryPath, source: absPath })) };
  }
  return { entries: [], error: `local path is neither a file nor a directory: ${absPath}` };
}
