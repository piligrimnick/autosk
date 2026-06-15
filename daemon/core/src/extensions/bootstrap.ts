/**
 * First-run environment bootstrap.
 *
 * autosk has **no daemon-bundled extensions**: the reference `feature-dev`
 * workflow (and its `@autosk/pi-agent` roles + `@autosk/worktree` isolation)
 * ship as ordinary npm packages. So on a brand-new machine — detected by the
 * ABSENCE of `~/.autosk/settings.json` — the daemon provisions the default
 * extensions itself: it `npm install`s them into the global packages prefix
 * (`~/.autosk/packages/`, the existing npm-extension discovery source) and
 * writes `~/.autosk/settings.json` listing them. Every project then discovers
 * `feature-dev` through the normal npm-packages source, with no per-project
 * files.
 *
 * `settings.json` IS the "already initialised" marker: once it exists this is a
 * no-op, so an operator who manages extensions by hand (or air-gapped) is never
 * surprised by a network install. A failed install deliberately leaves
 * `settings.json` absent so the next daemon start retries — the daemon keeps
 * serving either way (a bootstrap failure is logged, never fatal).
 *
 * The install shells out to `npm` (the compiled `autoskd` embeds the Bun
 * runtime but is NOT the Bun CLI, so it cannot `bun install` itself); the binary
 * is `$AUTOSK_NPM_BIN` or `npm` on `PATH`. Tests inject {@link BootstrapOptions.install}
 * to avoid touching the network.
 */

import { spawn } from "node:child_process";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

import { AUTOSK_DIR } from "../store/paths.ts";
import { consoleLogger, type Logger } from "../store/logger.ts";
import { readSettingsExtensions } from "./settings.ts";
import { classifySettingsEntry } from "./source.ts";

/**
 * Whether automatic npm installs are disabled via `AUTOSK_NO_AUTO_INSTALL`.
 *
 * When set to a truthy value (anything other than empty / `0` / `false`) the
 * daemon performs NO network installs at all: the first-run bootstrap and the
 * per-start reconcile both turn into no-ops, leaving listed-but-missing packages
 * as `project.diagnostics` only. This is the escape hatch for air-gapped or
 * hand-managed environments that want to provision `~/.autosk/packages/`
 * themselves.
 */
export function autoInstallDisabled(): boolean {
  const v = process.env.AUTOSK_NO_AUTO_INSTALL;
  return v != null && v !== "" && v !== "0" && v.toLowerCase() !== "false";
}

/**
 * npm install specifiers provisioned on first run. Installing `@autosk/feature-dev`
 * pulls its `@autosk/pi-agent` / `@autosk/worktree` / `@autosk/sdk` deps
 * transitively, so they need not be listed here.
 */
export const DEFAULT_BOOTSTRAP_PACKAGES = ["@autosk/feature-dev"] as const;

/**
 * Entries written to `settings.json#extensions` (the ones LOADED as extensions).
 * Uses the explicit `npm:` source form so first-run settings match what
 * `autosk ext add` writes. Only `feature-dev` registers a workflow; `pi-agent`
 * is a library its steps import, so it is installed (transitively) but not
 * listed here.
 */
export const DEFAULT_BOOTSTRAP_EXTENSIONS = ["npm:@autosk/feature-dev"] as const;

/** The actual installer; overridable in tests so the suite never hits npm. */
export type BootstrapInstaller = (args: {
  packagesDir: string;
  packages: string[];
}) => Promise<{ ok: boolean; error?: string }>;

export interface BootstrapOptions {
  /** Home dir whose `<home>/.autosk/{settings.json,packages}` are provisioned. */
  home: string;
  /** npm install specifiers (default {@link DEFAULT_BOOTSTRAP_PACKAGES}). */
  packages?: readonly string[];
  /** Names written to `settings.json#extensions` (default {@link DEFAULT_BOOTSTRAP_EXTENSIONS}). */
  extensions?: readonly string[];
  /** `npm` binary (default `$AUTOSK_NPM_BIN` or `npm`). */
  npmBin?: string;
  logger?: Logger;
  /** Override the install step (tests). Default: shell out to `npm`. */
  install?: BootstrapInstaller;
  /**
   * Override the registry version lookup used by `extension.update` (tests).
   * Default: shell out to `npm view`. Bootstrap/reconcile never call it; it is
   * threaded through here so the manager's {@link installerConfig} test seam can
   * inject a fake registry alongside `install`.
   */
  view?: NpmViewVersion;
}

export interface BootstrapResult {
  /** `skipped` (already initialised), `installed` (success), or `failed`. */
  status: "skipped" | "installed" | "failed";
  error?: string;
}

/** Options for the per-start {@link ensureExtensionsInstalled} reconcile. */
export interface ReconcileOptions {
  /** Packages prefix the listed npm extensions install into (`<scope>/.autosk/packages`). */
  packagesDir: string;
  /** `settings.json` paths to read `extensions` from (missing files ignored). */
  settingsPaths: string[];
  /** `npm` binary (default `$AUTOSK_NPM_BIN` or `npm`). */
  npmBin?: string;
  logger?: Logger;
  /** Override the install step (tests). Default: shell out to `npm`. */
  install?: BootstrapInstaller;
}

export interface ReconcileResult {
  /**
   * `skipped` (opted out via `AUTOSK_NO_AUTO_INSTALL`), `noop` (nothing listed /
   * nothing missing), `installed` (success), or `failed`.
   */
  status: "skipped" | "noop" | "installed" | "failed";
  /** The package names installed (present on `installed`). */
  installed?: string[];
  error?: string;
}

/**
 * Provisions the default extensions on first run (idempotent: a no-op once
 * `<home>/.autosk/settings.json` exists). Never throws — a failure is logged and
 * returned as `status:"failed"` so the daemon keeps serving.
 */
export async function ensureGlobalBootstrap(opts: BootstrapOptions): Promise<BootstrapResult> {
  const logger = opts.logger ?? consoleLogger;
  const autoskDir = join(opts.home, AUTOSK_DIR);
  const settingsPath = join(autoskDir, "settings.json");

  // settings.json is the "already initialised" marker.
  if (existsSync(settingsPath)) return { status: "skipped" };
  // `AUTOSK_NO_AUTO_INSTALL` opts out of every network install — leave
  // settings.json absent (so a later, opted-in start can still bootstrap).
  if (autoInstallDisabled()) {
    logger.info("autoskd: AUTOSK_NO_AUTO_INSTALL set — skipping first-run extension install");
    return { status: "skipped" };
  }

  const packages = [...(opts.packages ?? DEFAULT_BOOTSTRAP_PACKAGES)];
  const extensions = [...(opts.extensions ?? DEFAULT_BOOTSTRAP_EXTENSIONS)];
  const packagesDir = join(autoskDir, "packages");
  const install = opts.install ?? npmInstaller(opts.npmBin, logger);

  logger.info(`autoskd: first run — installing default extensions (${packages.join(", ")}) into ${packagesDir}`);
  try {
    mkdirSync(packagesDir, { recursive: true });
    ensurePackagesManifest(packagesDir);

    const res = await install({ packagesDir, packages });
    if (!res.ok) {
      logger.error(
        `autoskd: first-run extension install failed (${res.error ?? "unknown error"}) — ` +
          `leaving settings.json absent so it retries on next start`,
      );
      return { status: "failed", error: res.error };
    }

    writeSettings(settingsPath, extensions);
    logger.info(`autoskd: first-run init complete — wrote ${settingsPath} (extensions: ${extensions.join(", ")})`);
    return { status: "installed" };
  } catch (e) {
    const error = e instanceof Error ? e.message : String(e);
    logger.error(`autoskd: first-run init error: ${error}`);
    return { status: "failed", error };
  }
}

/**
 * Per-start reconcile: install any `npm:` package listed under `extensions` in
 * the given `settings.json` files that is NOT yet present under
 * `<packagesDir>/node_modules/`. Only the **missing** packages are installed —
 * already-installed ones are left untouched (no upgrade), so a fully-provisioned
 * environment never hits the network. Local-path entries are skipped (they
 * resolve in place); unrecognised entries are skipped (the loader diagnoses them).
 *
 * Used on two seams: the global `~/.autosk/settings.json` at daemon start (with
 * `packagesDir = ~/.autosk/packages`), and each project's `./.autosk/settings.json`
 * on first project open (with `packagesDir = <root>/.autosk/packages`). Never
 * throws — a failed install is logged and the listed-but-missing package simply
 * stays a `project.diagnostics` entry until the next start retries.
 */
export async function ensureExtensionsInstalled(opts: ReconcileOptions): Promise<ReconcileResult> {
  const logger = opts.logger ?? consoleLogger;
  if (autoInstallDisabled()) return { status: "skipped" };

  const packagesDir = opts.packagesDir;
  const nodeModules = join(packagesDir, "node_modules");
  const home = process.env.HOME ?? "";

  // Union of every listed npm package across the given settings files, in
  // first-seen order, keyed by name (a missing/corrupt settings file contributes
  // nothing; local + unrecognised entries are skipped here).
  const wanted: { name: string; spec: string }[] = [];
  const seen = new Set<string>();
  for (const settingsPath of opts.settingsPaths) {
    const baseDir = dirname(settingsPath);
    for (const entry of readSettingsExtensions(settingsPath)) {
      const classified = classifySettingsEntry(entry, { baseDir, home });
      if (classified.kind !== "npm") continue;
      if (seen.has(classified.name)) continue;
      seen.add(classified.name);
      wanted.push({ name: classified.name, spec: classified.spec });
    }
  }
  if (wanted.length === 0) return { status: "noop" };

  // A package is "installed" iff its node_modules dir exists (the same check the
  // discovery loader uses to decide a package resolves vs. raises a diagnostic).
  const missing = wanted.filter((w) => !existsSync(join(nodeModules, w.name)));
  if (missing.length === 0) return { status: "noop" };

  const install = opts.install ?? npmInstaller(opts.npmBin, logger);
  const names = missing.map((m) => m.name);
  logger.info(`autoskd: installing missing extension package(s): ${names.join(", ")} into ${packagesDir}`);
  try {
    mkdirSync(packagesDir, { recursive: true });
    ensurePackagesManifest(packagesDir);

    const res = await install({ packagesDir, packages: missing.map((m) => m.spec) });
    if (!res.ok) {
      logger.error(
        `autoskd: extension install failed (${res.error ?? "unknown error"}) — ` +
          `listed-but-missing package(s) stay diagnostics until the next start retries`,
      );
      return { status: "failed", error: res.error, installed: [] };
    }
    logger.info(`autoskd: installed missing extension package(s): ${names.join(", ")}`);
    return { status: "installed", installed: names };
  } catch (e) {
    const error = e instanceof Error ? e.message : String(e);
    logger.error(`autoskd: extension reconcile error: ${error}`);
    return { status: "failed", error };
  }
}

/** Writes a minimal npm manifest so `npm install` has a project to save into. */
export function ensurePackagesManifest(packagesDir: string): void {
  const manifest = join(packagesDir, "package.json");
  if (existsSync(manifest)) return;
  writeFileSync(
    manifest,
    `${JSON.stringify({ name: "autosk-packages", version: "0.0.0", private: true }, null, 2)}\n`,
    "utf8",
  );
}

/** Writes `settings.json` with the loaded-extension list. */
function writeSettings(settingsPath: string, extensions: string[]): void {
  writeFileSync(settingsPath, `${JSON.stringify({ extensions }, null, 2)}\n`, "utf8");
}

/**
 * Looks up a package's latest registry version. Overridable in tests so the
 * suite never hits the network (the `view` sibling of {@link BootstrapInstaller}).
 */
export type NpmViewVersion = (
  name: string,
) => Promise<{ ok: boolean; version?: string; error?: string }>;

/**
 * The default registry lookup: `npm view <name> version --json` (10s timeout),
 * a capturing sibling of {@link npmInstaller}. Mirrors pi's update check. A
 * non-zero exit, spawn failure, timeout, or unparseable output resolves
 * `{ ok: false }` so callers can fail-open.
 */
export function npmViewVersion(npmBin: string | undefined, logger: Logger): NpmViewVersion {
  const bin = npmBin ?? process.env.AUTOSK_NPM_BIN ?? "npm";
  return (name) =>
    new Promise((resolve) => {
      const args = ["view", name, "version", "--json"];
      let child;
      try {
        child = spawn(bin, args, { stdio: ["ignore", "pipe", "pipe"] });
      } catch (e) {
        resolve({ ok: false, error: `spawn ${bin}: ${e instanceof Error ? e.message : String(e)}` });
        return;
      }
      let stdout = "";
      let stderr = "";
      const timer = setTimeout(() => {
        try {
          child.kill("SIGKILL");
        } catch {
          /* already gone */
        }
        resolve({ ok: false, error: `npm view ${name} timed out` });
      }, 10_000);
      child.stdout?.on("data", (d: Buffer) => {
        stdout += d.toString();
      });
      child.stderr?.on("data", (d: Buffer) => {
        stderr += d.toString();
      });
      child.on("error", (e) => {
        clearTimeout(timer);
        resolve({ ok: false, error: `spawn ${bin}: ${e.message}` });
      });
      child.on("close", (code) => {
        clearTimeout(timer);
        if (code !== 0) {
          resolve({ ok: false, error: `${bin} view exited ${code}: ${stderr.trim().slice(-600)}` });
          return;
        }
        const version = parseNpmViewVersion(stdout);
        if (version === undefined) {
          resolve({ ok: false, error: `could not parse npm view output for ${name}` });
          return;
        }
        resolve({ ok: true, version });
      });
      void logger; // reserved for future verbose logging
    });
}

/**
 * Parses `npm view <name> version --json` stdout. A single matching version is a
 * JSON string (`"1.2.3"`); when several dist-tags/versions match npm emits an
 * array — take the last (highest) entry. Anything else ⇒ `undefined`.
 */
function parseNpmViewVersion(stdout: string): string | undefined {
  const trimmed = stdout.trim();
  if (trimmed.length === 0) return undefined;
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return undefined;
  }
  if (typeof parsed === "string") return parsed;
  if (Array.isArray(parsed)) {
    const last = parsed[parsed.length - 1];
    return typeof last === "string" ? last : undefined;
  }
  return undefined;
}

/** The default installer: `npm install <packages…>` inside the packages prefix. */
export function npmInstaller(npmBin: string | undefined, logger: Logger): BootstrapInstaller {
  const bin = npmBin ?? process.env.AUTOSK_NPM_BIN ?? "npm";
  return ({ packagesDir, packages }) =>
    new Promise((resolve) => {
      const args = ["install", ...packages, "--no-audit", "--no-fund"];
      let child;
      try {
        child = spawn(bin, args, { cwd: packagesDir, stdio: ["ignore", "pipe", "pipe"] });
      } catch (e) {
        resolve({ ok: false, error: `spawn ${bin}: ${e instanceof Error ? e.message : String(e)}` });
        return;
      }
      let stderr = "";
      child.stdout?.on("data", () => {
        /* discard npm progress; failures carry stderr */
      });
      child.stderr?.on("data", (d: Buffer) => {
        stderr += d.toString();
      });
      child.on("error", (e) => resolve({ ok: false, error: `spawn ${bin}: ${e.message}` }));
      child.on("close", (code) => {
        if (code === 0) resolve({ ok: true });
        else resolve({ ok: false, error: `${bin} exited ${code}: ${stderr.trim().slice(-600)}` });
      });
      void logger; // reserved for future verbose logging
    });
}
