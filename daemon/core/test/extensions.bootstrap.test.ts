/**
 * First-run environment bootstrap (replaces the old daemon-bundled route).
 *
 * autosk ships NO bundled extensions: on a fresh machine (no
 * `~/.autosk/settings.json`) the daemon `npm install`s the reference
 * `@autosk/feature-dev` workflow into `~/.autosk/packages/` and writes
 * `settings.json`, so every project then discovers it through the normal
 * npm-packages source with no per-project files.
 *
 * These tests inject a link-based installer (symlinking the source
 * `@autosk/feature-dev` package into the fake packages prefix) so the bootstrap
 * + discovery path is exercised end-to-end WITHOUT touching the network.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, symlinkSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  ProjectManager,
  ProjectRegistry,
  ensureGlobalBootstrap,
  initProject,
  loadProjectRegistry,
  type BootstrapInstaller,
} from "../src/index.ts";
import { tempDir } from "./helpers.ts";

/** The shipped `@autosk/feature-dev` source package (its `autosk.extensions` → `./index.ts`). */
const FEATURE_DEV_SRC = fileURLToPath(new URL("../../extensions/feature-dev", import.meta.url));

/**
 * A fake installer that simulates `npm install @autosk/feature-dev` by symlinking
 * the source package into `<packagesDir>/node_modules/@autosk/feature-dev`. Its
 * `@autosk/*` deps resolve from the workspace `node_modules` at import time (the
 * test runs interpreted, not as a compiled binary), so they need not be linked.
 */
function linkInstaller(): { install: BootstrapInstaller; calls: () => number } {
  let n = 0;
  const install: BootstrapInstaller = async ({ packagesDir }) => {
    n++;
    const scope = join(packagesDir, "node_modules", "@autosk");
    mkdirSync(scope, { recursive: true });
    symlinkSync(FEATURE_DEV_SRC, join(scope, "feature-dev"));
    return { ok: true };
  };
  return { install, calls: () => n };
}

describe("first-run bootstrap", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  test("provisions settings.json + the default extension, idempotently", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const { install, calls } = linkInstaller();

    const first = await ensureGlobalBootstrap({ home: home.path, install });
    expect(first.status).toBe("installed");

    const settingsPath = join(home.path, ".autosk", "settings.json");
    expect(existsSync(settingsPath)).toBe(true);
    expect(JSON.parse(readFileSync(settingsPath, "utf8"))).toEqual({ extensions: ["@autosk/feature-dev"] });
    expect(calls()).toBe(1);

    // settings.json is the "already initialised" marker → a second run is a no-op.
    const second = await ensureGlobalBootstrap({ home: home.path, install });
    expect(second.status).toBe("skipped");
    expect(calls()).toBe(1);
  });

  test("a failed install leaves settings.json absent so it retries next start", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const install: BootstrapInstaller = async () => ({ ok: false, error: "npm boom" });

    const res = await ensureGlobalBootstrap({ home: home.path, install });
    expect(res.status).toBe("failed");
    expect(res.error).toContain("npm boom");
    expect(existsSync(join(home.path, ".autosk", "settings.json"))).toBe(false);
  });

  test("a fresh project discovers feature-dev after bootstrap (npm-packages source)", async () => {
    const project = tempDir();
    const home = tempDir();
    cleanups.push(project.cleanup, home.cleanup);
    await initProject(project.path);

    const { install } = linkInstaller();
    await ensureGlobalBootstrap({ home: home.path, install });

    const registry = await loadProjectRegistry(project.path, { home: home.path });
    const wf = registry.getWorkflowInfo("feature-dev");
    expect(wf).toBeDefined();
    expect(wf!.first_step).toBe("dev");
    expect(wf!.isolation).toBe("worktree");
    expect(wf!.steps.map((s) => s.name).sort()).toEqual(["accept", "dev", "docs", "review", "validator"]);
    expect(registry.diagnostics).toEqual([]);
  });

  test("ProjectManager runs bootstrap exactly once before building a project registry", async () => {
    const project = tempDir();
    const home = tempDir();
    cleanups.push(project.cleanup, home.cleanup);
    await initProject(project.path);

    const { install, calls } = linkInstaller();
    const pm = new ProjectManager({
      registry: new ProjectRegistry(`${home.path}/.autosk/projects.json`),
      store: { watch: false },
      extensions: { home: home.path },
      bootstrap: { install },
    });
    try {
      const handle = await pm.resolve(project.path);
      expect(handle.extensions.resolveWorkflow("feature-dev")).toBeDefined();
      expect(calls()).toBe(1);
      // A second open does NOT re-bootstrap (single-flight memoised).
      await pm.open(handle.root);
      expect(calls()).toBe(1);
    } finally {
      await pm.close();
    }
  });

  test("ProjectManager with no bootstrap config never installs", async () => {
    const project = tempDir();
    const home = tempDir();
    cleanups.push(project.cleanup, home.cleanup);
    await initProject(project.path);

    const pm = new ProjectManager({
      registry: new ProjectRegistry(`${home.path}/.autosk/projects.json`),
      store: { watch: false },
      extensions: { home: home.path },
      // no `bootstrap` → disabled (the test default)
    });
    try {
      const handle = await pm.resolve(project.path);
      expect(handle.extensions.resolveWorkflow("feature-dev")).toBeUndefined();
      expect(existsSync(join(home.path, ".autosk", "settings.json"))).toBe(false);
    } finally {
      await pm.close();
    }
  });
});
