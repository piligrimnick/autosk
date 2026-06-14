/**
 * Per-start extension reconcile (the "auto-install missing packages" path).
 *
 * Unlike the first-run bootstrap (which only fires when `~/.autosk/settings.json`
 * is ABSENT), `ensureExtensionsInstalled` runs on every start and installs any
 * package listed under `settings.json#extensions` that is not yet present under
 * `~/.autosk/packages/node_modules/`. Only the MISSING packages install — a
 * fully-provisioned environment never hits the (here, faked) installer.
 *
 * These tests inject a dir-based installer (it just `mkdir`s the package dir
 * the real `npm install` would create) so the reconcile + discovery path is
 * exercised WITHOUT touching the network.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import {
  ProjectManager,
  ProjectRegistry,
  ensureExtensionsInstalled,
  initProject,
  type BootstrapInstaller,
} from "../src/index.ts";
import { tempDir } from "./helpers.ts";

/** Writes `<dir>/.autosk/settings.json` with the given extension package list. */
function writeSettings(dir: string, extensions: string[]): string {
  const autoskDir = join(dir, ".autosk");
  mkdirSync(autoskDir, { recursive: true });
  const path = join(autoskDir, "settings.json");
  writeFileSync(path, `${JSON.stringify({ extensions }, null, 2)}\n`, "utf8");
  return path;
}

/** Marks `<home>/.autosk/node_modules/<pkg>` as already installed. */
function markInstalled(home: string, pkg: string): void {
  const dir = join(home, ".autosk", "packages", "node_modules", pkg);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "package.json"), JSON.stringify({ name: pkg, version: "0.0.0" }), "utf8");
}

/** A fake installer that simulates `npm install` by creating each package dir. */
function dirInstaller(): { install: BootstrapInstaller; calls: () => string[][] } {
  const calls: string[][] = [];
  const install: BootstrapInstaller = async ({ packagesDir, packages }) => {
    calls.push([...packages]);
    for (const pkg of packages) {
      const dir = join(packagesDir, "node_modules", pkg);
      mkdirSync(dir, { recursive: true });
      writeFileSync(join(dir, "package.json"), JSON.stringify({ name: pkg, version: "1.0.0" }), "utf8");
    }
    return { ok: true };
  };
  return { install, calls: () => calls };
}

describe("per-start extension reconcile", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
    delete process.env.AUTOSK_NO_AUTO_INSTALL;
  });

  test("installs only the listed packages that are missing", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const settings = writeSettings(home.path, ["@me/flow-a", "@me/flow-b"]);
    markInstalled(home.path, "@me/flow-a"); // already present

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({ home: home.path, settingsPaths: [settings], install });

    expect(res.status).toBe("installed");
    expect(res.installed).toEqual(["@me/flow-b"]);
    expect(calls()).toEqual([["@me/flow-b"]]);
    expect(existsSync(join(home.path, ".autosk", "packages", "node_modules", "@me", "flow-b"))).toBe(true);
  });

  test("is a no-op when every listed package is already installed", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const settings = writeSettings(home.path, ["@me/flow-a"]);
    markInstalled(home.path, "@me/flow-a");

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({ home: home.path, settingsPaths: [settings], install });

    expect(res.status).toBe("noop");
    expect(calls()).toEqual([]);
  });

  test("is a no-op when nothing is listed (missing settings file)", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({
      home: home.path,
      settingsPaths: [join(home.path, ".autosk", "settings.json")],
      install,
    });
    expect(res.status).toBe("noop");
    expect(calls()).toEqual([]);
  });

  test("AUTOSK_NO_AUTO_INSTALL opts out of the install entirely", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const settings = writeSettings(home.path, ["@me/flow-a"]);
    process.env.AUTOSK_NO_AUTO_INSTALL = "1";

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({ home: home.path, settingsPaths: [settings], install });

    expect(res.status).toBe("skipped");
    expect(calls()).toEqual([]);
    expect(existsSync(join(home.path, ".autosk", "packages", "node_modules", "@me", "flow-a"))).toBe(false);
  });

  test("merges (deduped, first-seen order) across multiple settings files", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    const globalSettings = writeSettings(home.path, ["@me/flow-a"]);
    const projectSettings = writeSettings(project.path, ["@me/flow-a", "@me/flow-b"]);

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({
      home: home.path,
      settingsPaths: [globalSettings, projectSettings],
      install,
    });

    expect(res.status).toBe("installed");
    // @me/flow-a appears in both files but is requested once, in first-seen order.
    expect(res.installed).toEqual(["@me/flow-a", "@me/flow-b"]);
    expect(calls()).toEqual([["@me/flow-a", "@me/flow-b"]]);
  });

  test("ProjectManager installs a project-local settings.json package on open", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    // Pre-write the GLOBAL settings.json so first-run bootstrap is skipped and
    // the global reconcile is a no-op — isolating the project reconcile.
    writeSettings(home.path, []);
    writeSettings(project.path, ["@me/proj-flow"]);

    const { install, calls } = dirInstaller();
    const pm = new ProjectManager({
      registry: new ProjectRegistry(`${home.path}/.autosk/projects.json`),
      store: { watch: false },
      extensions: { home: home.path },
      bootstrap: { install },
    });
    try {
      await pm.resolve(project.path);
      // The project's listed-but-missing package was installed on first open.
      expect(calls()).toContainEqual(["@me/proj-flow"]);
      expect(existsSync(join(home.path, ".autosk", "packages", "node_modules", "@me", "proj-flow"))).toBe(true);
    } finally {
      await pm.close();
    }
  });

  test("ProjectManager with no bootstrap config never reconciles", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    writeSettings(project.path, ["@me/proj-flow"]);

    const pm = new ProjectManager({
      registry: new ProjectRegistry(`${home.path}/.autosk/projects.json`),
      store: { watch: false },
      extensions: { home: home.path },
      // no `bootstrap` → reconcile disabled (the test default)
    });
    try {
      await pm.resolve(project.path);
      expect(existsSync(join(home.path, ".autosk", "packages", "node_modules", "@me", "proj-flow"))).toBe(false);
    } finally {
      await pm.close();
    }
  });
});
