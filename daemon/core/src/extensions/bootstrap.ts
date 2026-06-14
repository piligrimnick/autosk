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
