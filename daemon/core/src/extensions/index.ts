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
  resolveLocalPath,
  readAutoskExtensionPaths,
  isExtensionFile,
  type ExtensionEntry,
  type PackageResolution,
} from "./discovery.ts";
export {
  readSettingsExtensions,
  upsertExtensionEntry,
  removeExtensionEntry,
} from "./settings.ts";
export {
  parseInstallSource,
  classifySettingsEntry,
  npmName,
  isLocalPathSpec,
  settingsEntryFor,
  sameSource,
  InvalidExtensionSourceError,
  type ExtensionSource,
  type ClassifiedEntry,
  type SourceContext,
  type SettingsContext,
} from "./source.ts";
export {
  installExtension,
  removeExtensionFromSettings,
  listExtensionEntries,
  type InstallExtensionOptions,
  type InstallExtensionResult,
  type RemoveExtensionOptions,
  type ExtensionEntryInfo,
  type ListExtensionsOptions,
} from "./install.ts";
export {
  updateExtensions,
  type UpdateExtensionsOptions,
} from "./update.ts";
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
  ensurePackagesManifest,
  npmInstaller,
  npmViewVersion,
  autoInstallDisabled,
  DEFAULT_BOOTSTRAP_PACKAGES,
  DEFAULT_BOOTSTRAP_EXTENSIONS,
  type BootstrapOptions,
  type BootstrapInstaller,
  type BootstrapResult,
  type NpmViewVersion,
  type ReconcileOptions,
  type ReconcileResult,
} from "./bootstrap.ts";
