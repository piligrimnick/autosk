/**
 * Per-start extension reconcile (the "auto-install missing packages" path).
 *
 * Unlike the first-run bootstrap (which only fires when `~/.autosk/settings.json`
 * is ABSENT), `ensureExtensionsInstalled` runs on every start and installs any
 * `npm:` package listed under `settings.json#extensions` that is not yet present
 * under `<packagesDir>/node_modules/`. Only the MISSING packages install — a
 * fully-provisioned environment never hits the (here, faked) installer. Local
 * and unrecognised entries are skipped (they resolve in place / are diagnosed by
 * the loader).
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

/** The packages prefix under a given home/project dir. */
function packagesDirOf(dir: string): string {
  return join(dir, ".autosk", "packages");
}

/** Writes `<dir>/.autosk/settings.json` with the given extension entry list. */
function writeSettings(dir: string, extensions: string[]): string {
  const autoskDir = join(dir, ".autosk");
  mkdirSync(autoskDir, { recursive: true });
  const path = join(autoskDir, "settings.json");
  writeFileSync(path, `${JSON.stringify({ extensions }, null, 2)}\n`, "utf8");
  return path;
}

/** Marks `<packagesDir>/node_modules/<pkg>` as already installed. */
function markInstalled(packagesDir: string, pkg: string): void {
  const dir = join(packagesDir, "node_modules", pkg);
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
    const settings = writeSettings(home.path, ["npm:@me/flow-a", "npm:@me/flow-b"]);
    const packagesDir = packagesDirOf(home.path);
    markInstalled(packagesDir, "@me/flow-a"); // already present

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({ packagesDir, settingsPaths: [settings], install });

    expect(res.status).toBe("installed");
    expect(res.installed).toEqual(["@me/flow-b"]);
    expect(calls()).toEqual([["@me/flow-b"]]);
    expect(existsSync(join(packagesDir, "node_modules", "@me", "flow-b"))).toBe(true);
  });

  test("pins the npm version from the spec but checks presence by name", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const settings = writeSettings(home.path, ["npm:@me/flow-a@1.2.3"]);
    const packagesDir = packagesDirOf(home.path);

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({ packagesDir, settingsPaths: [settings], install });

    expect(res.status).toBe("installed");
    // installed reports the NAME; the installer receives the versioned SPEC.
    expect(res.installed).toEqual(["@me/flow-a"]);
    expect(calls()).toEqual([["@me/flow-a@1.2.3"]]);
  });

  test("skips local-path and unrecognised entries", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const settings = writeSettings(home.path, ["/abs/local-ext", "bare-name", "npm:@me/real"]);
    const packagesDir = packagesDirOf(home.path);

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({ packagesDir, settingsPaths: [settings], install });

    // Only the npm entry installs; the path + bare entries are left to the loader.
    expect(res.status).toBe("installed");
    expect(res.installed).toEqual(["@me/real"]);
    expect(calls()).toEqual([["@me/real"]]);
  });

  test("is a no-op when every listed package is already installed", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const settings = writeSettings(home.path, ["npm:@me/flow-a"]);
    markInstalled(packagesDirOf(home.path), "@me/flow-a");

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({
      packagesDir: packagesDirOf(home.path),
      settingsPaths: [settings],
      install,
    });

    expect(res.status).toBe("noop");
    expect(calls()).toEqual([]);
  });

  test("is a no-op when nothing is listed (missing settings file)", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({
      packagesDir: packagesDirOf(home.path),
      settingsPaths: [join(home.path, ".autosk", "settings.json")],
      install,
    });
    expect(res.status).toBe("noop");
    expect(calls()).toEqual([]);
  });

  test("AUTOSK_NO_AUTO_INSTALL opts out of the install entirely", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const settings = writeSettings(home.path, ["npm:@me/flow-a"]);
    process.env.AUTOSK_NO_AUTO_INSTALL = "1";

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({
      packagesDir: packagesDirOf(home.path),
      settingsPaths: [settings],
      install,
    });

    expect(res.status).toBe("skipped");
    expect(calls()).toEqual([]);
    expect(existsSync(join(packagesDirOf(home.path), "node_modules", "@me", "flow-a"))).toBe(false);
  });

  test("merges (deduped, first-seen order) across multiple settings files", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    const globalSettings = writeSettings(home.path, ["npm:@me/flow-a"]);
    const projectSettings = writeSettings(project.path, ["npm:@me/flow-a", "npm:@me/flow-b"]);
    const packagesDir = packagesDirOf(home.path);

    const { install, calls } = dirInstaller();
    const res = await ensureExtensionsInstalled({
      packagesDir,
      settingsPaths: [globalSettings, projectSettings],
      install,
    });

    expect(res.status).toBe("installed");
    // @me/flow-a appears in both files but is requested once, in first-seen order.
    expect(res.installed).toEqual(["@me/flow-a", "@me/flow-b"]);
    expect(calls()).toEqual([["@me/flow-a", "@me/flow-b"]]);
  });

  test("ProjectManager installs a project-local settings.json package into the project packages dir", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    // Pre-write the GLOBAL settings.json so first-run bootstrap is skipped and
    // the global reconcile is a no-op — isolating the project reconcile.
    writeSettings(home.path, []);
    writeSettings(project.path, ["npm:@me/proj-flow"]);

    const { install, calls } = dirInstaller();
    const pm = new ProjectManager({
      registry: new ProjectRegistry(`${home.path}/.autosk/projects.json`),
      store: { watch: false },
      extensions: { home: home.path },
      bootstrap: { install },
    });
    try {
      await pm.resolve(project.path);
      // The project's listed-but-missing package installed into the PROJECT's
      // own packages dir (consistent with the loader's scope-aware resolution).
      expect(calls()).toContainEqual(["@me/proj-flow"]);
      expect(existsSync(join(packagesDirOf(project.path), "node_modules", "@me", "proj-flow"))).toBe(true);
      // And NOT into the global packages dir.
      expect(existsSync(join(packagesDirOf(home.path), "node_modules", "@me", "proj-flow"))).toBe(false);
    } finally {
      await pm.close();
    }
  });

  test("ProjectManager with no bootstrap config never reconciles", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    writeSettings(project.path, ["npm:@me/proj-flow"]);

    const pm = new ProjectManager({
      registry: new ProjectRegistry(`${home.path}/.autosk/projects.json`),
      store: { watch: false },
      extensions: { home: home.path },
      // no `bootstrap` → reconcile disabled (the test default)
    });
    try {
      await pm.resolve(project.path);
      expect(existsSync(join(packagesDirOf(project.path), "node_modules", "@me", "proj-flow"))).toBe(false);
    } finally {
      await pm.close();
    }
  });
});
