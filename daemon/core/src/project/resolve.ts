/**
 * Project-root resolution (plan §3.7(1)).
 *
 * A project is any directory that contains a `.autosk/` directory. Resolution
 * walks up from `{cwd}` to the nearest such directory (an explicit `override`
 * short-circuits the walk and is required to contain `.autosk/` itself). The
 * resolved root is canonicalised (symlinks resolved) so the project cache and
 * the registry agree on one key per project.
 *
 * Resolution is a pure read: it NEVER mutates `~/.autosk/projects.json`
 * (acceptance: "unknown project dirs are NOT auto-registered by reads").
 */

import { realpath, stat } from "node:fs/promises";
import { dirname, isAbsolute, join, resolve } from "node:path";

import { AUTOSK_DIR } from "../store/paths.ts";

/** No `.autosk/` directory was found from the selector. */
export class ProjectNotFoundError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ProjectNotFoundError";
  }
}

/** The selector was malformed (empty / non-absolute cwd). */
export class InvalidProjectError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "InvalidProjectError";
  }
}

async function isDir(p: string): Promise<boolean> {
  try {
    return (await stat(p)).isDirectory();
  } catch {
    return false;
  }
}

/** Canonicalises a path: `realpath` when it exists, else a lexical `resolve`. */
export async function canonicalize(p: string): Promise<string> {
  try {
    return await realpath(p);
  } catch {
    return resolve(p);
  }
}

/**
 * Resolves a project root. `override` (when non-empty) wins and must itself
 * contain `.autosk/`; otherwise walk up from the absolute `cwd` to the nearest
 * directory containing `.autosk/`.
 */
export async function resolveProjectRoot(cwd: string, override?: string): Promise<string> {
  if (override && override.length > 0) {
    const root = resolve(override);
    if (!(await isDir(join(root, AUTOSK_DIR)))) {
      throw new ProjectNotFoundError(`no ${AUTOSK_DIR}/ at ${root}`);
    }
    return canonicalize(root);
  }

  if (!cwd || !isAbsolute(cwd)) {
    throw new InvalidProjectError(`cwd must be an absolute path (got ${JSON.stringify(cwd)})`);
  }

  let dir = resolve(cwd);
  for (;;) {
    if (await isDir(join(dir, AUTOSK_DIR))) return canonicalize(dir);
    const parent = dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  throw new ProjectNotFoundError(`no ${AUTOSK_DIR}/ found from ${cwd} or any parent`);
}
