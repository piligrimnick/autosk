/**
 * `project.init` — materialise a fresh `.autosk/` skeleton (plan §3.7(1)).
 *
 * Creates `tasks/`, `sessions/`, and `extensions/` under `<dir>/.autosk/`.
 * Init does NOT register the project in `~/.autosk/projects.json` — registration
 * is the explicit job of `project.add`; init just lays down the on-disk skeleton
 * and returns the project's identity.
 */

import { mkdir } from "node:fs/promises";
import { basename, resolve } from "node:path";

import type { ProjectInfo } from "@autosk/sdk";
import { ProjectPaths } from "../store/paths.ts";
import { canonicalize } from "./resolve.ts";

/** Creates the `.autosk/` skeleton under `dir` and returns its `ProjectInfo`. */
export async function initProject(dir: string): Promise<ProjectInfo> {
  const paths = new ProjectPaths(resolve(dir));
  for (const d of paths.skeletonDirs()) {
    await mkdir(d, { recursive: true });
  }
  const root = await canonicalize(paths.root);
  return { root, name: basename(root) };
}
