/**
 * The project manager (plan §3.7(1)): registry, walk-up resolution, lazy open.
 */

export {
  ProjectManager,
  type ProjectHandle,
  type ProjectManagerOptions,
  type RebuildRegistryResult,
} from "./manager.ts";
export { ProjectRegistry } from "./registry.ts";
export { initProject } from "./init.ts";
export {
  resolveProjectRoot,
  canonicalize,
  ProjectNotFoundError,
  InvalidProjectError,
} from "./resolve.ts";
