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
import { join } from "node:path";

import { AUTOSK_DIR } from "../store/paths.ts";
import { consoleLogger, type Logger } from "../store/logger.ts";
import { readSettingsExtensions } from "./settings.ts";

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
 * Package names written to `settings.json#extensions` (the ones LOADED as
 * extensions). Only `feature-dev` registers a workflow; `pi-agent` is a library
 * its steps import, so it is installed (transitively) but not listed here.
 */
export const DEFAULT_BOOTSTRAP_EXTENSIONS = ["@autosk/feature-dev"] as const;

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
}

export interface BootstrapResult {
  /** `skipped` (already initialised), `installed` (success), or `failed`. */
  status: "skipped" | "installed" | "failed";
  error?: string;
}

/** Options for the per-start {@link ensureExtensionsInstalled} reconcile. */
export interface ReconcileOptions {
  /** Home dir whose `<home>/.autosk/packages` is the install prefix. */
  home: string;
  /** `settings.json` paths to read `extensions` package names from (missing files ignored). */
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
 * Per-start reconcile: install any package listed under `extensions` in the
 * given `settings.json` files that is NOT yet present under
 * `~/.autosk/packages/node_modules/`. Only the **missing** packages are
 * installed — already-installed ones are left untouched (no upgrade), so a
 * fully-provisioned environment never hits the network.
 *
 * Used on two seams: the global `~/.autosk/settings.json` at daemon start, and
 * each project's `./.autosk/settings.json` on first project open. Never throws —
 * a failed install is logged and the listed-but-missing package simply stays a
 * `project.diagnostics` entry until the next start retries.
 */
export async function ensureExtensionsInstalled(opts: ReconcileOptions): Promise<ReconcileResult> {
  const logger = opts.logger ?? consoleLogger;
  if (autoInstallDisabled()) return { status: "skipped" };

  const packagesDir = join(opts.home, AUTOSK_DIR, "packages");
  const nodeModules = join(packagesDir, "node_modules");

  // Union of every listed package name across the given settings files, in
  // first-seen order (a missing/corrupt settings file contributes nothing).
  const names: string[] = [];
  const seen = new Set<string>();
  for (const settingsPath of opts.settingsPaths) {
    for (const name of readSettingsExtensions(settingsPath)) {
      if (seen.has(name)) continue;
      seen.add(name);
      names.push(name);
    }
  }
  if (names.length === 0) return { status: "noop" };

  // A package is "installed" iff its node_modules dir exists (the same check the
  // discovery loader uses to decide a package resolves vs. raises a diagnostic).
  const missing = names.filter((name) => !existsSync(join(nodeModules, name)));
  if (missing.length === 0) return { status: "noop" };

  const install = opts.install ?? npmInstaller(opts.npmBin, logger);
  logger.info(`autoskd: installing missing extension package(s): ${missing.join(", ")} into ${packagesDir}`);
  try {
    mkdirSync(packagesDir, { recursive: true });
    ensurePackagesManifest(packagesDir);

    const res = await install({ packagesDir, packages: missing });
    if (!res.ok) {
      logger.error(
        `autoskd: extension install failed (${res.error ?? "unknown error"}) — ` +
          `listed-but-missing package(s) stay diagnostics until the next start retries`,
      );
      return { status: "failed", error: res.error, installed: [] };
    }
    logger.info(`autoskd: installed missing extension package(s): ${missing.join(", ")}`);
    return { status: "installed", installed: missing };
  } catch (e) {
    const error = e instanceof Error ? e.message : String(e);
    logger.error(`autoskd: extension reconcile error: ${error}`);
    return { status: "failed", error };
  }
}

/** Writes a minimal npm manifest so `npm install` has a project to save into. */
function ensurePackagesManifest(packagesDir: string): void {
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

/** The default installer: `npm install <packages…>` inside the packages prefix. */
function npmInstaller(npmBin: string | undefined, logger: Logger): BootstrapInstaller {
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
