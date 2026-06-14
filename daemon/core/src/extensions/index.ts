/**
 * The extension system (plan §3.6): pi-style discovery, in-process loading with
 * full error isolation, per-project registries, and the live-code hazard guard.
 */

export {
  ExtensionRegistry,
  renderWorkflowInfo,
  type PositionValidation,
} from "./registry.ts";
export {
  discoverDir,
  resolveExtensionEntries,
  resolvePackageEntries,
  readAutoskExtensionPaths,
  type ExtensionEntry,
  type PackageResolution,
} from "./discovery.ts";
export { readSettingsExtensions } from "./settings.ts";
export {
  loadProjectRegistry,
  resolveProjectEntries,
  type ExtensionEnv,
  type ResolvedProjectEntries,
} from "./loader.ts";
export {
  validateInFlightTasks,
  HAZARD_COMMENT_AUTHOR,
  type ParkedTask,
} from "./hazard.ts";
export {
  ensureGlobalBootstrap,
  ensureExtensionsInstalled,
  autoInstallDisabled,
  DEFAULT_BOOTSTRAP_PACKAGES,
  DEFAULT_BOOTSTRAP_EXTENSIONS,
  type BootstrapOptions,
  type BootstrapInstaller,
  type BootstrapResult,
  type ReconcileOptions,
  type ReconcileResult,
} from "./bootstrap.ts";
