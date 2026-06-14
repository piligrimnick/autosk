/**
 * Explicit extension management — `installExtension` / `removeExtensionFromSettings`
 * / `listExtensionEntries`, plus the ProjectManager integration (install / list /
 * remove, global + project `-l` scopes).
 *
 * The npm path uses a fake installer (mkdir the package dir) so nothing hits the
 * network; the local path is exercised in place (never copied).
 */

import { afterEach, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, realpathSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import {
  ProjectManager,
  ProjectRegistry,
  initProject,
  installExtension,
  listExtensionEntries,
  parseInstallSource,
  readSettingsExtensions,
  removeExtensionFromSettings,
  type BootstrapInstaller,
} from "../src/index.ts";
import { tempDir } from "./helpers.ts";

/** A fake installer that simulates `npm install` by creating each package dir. */
function dirInstaller(): { install: BootstrapInstaller; calls: () => string[][] } {
  const calls: string[][] = [];
  const install: BootstrapInstaller = async ({ packagesDir, packages }) => {
    calls.push([...packages]);
    for (const pkg of packages) {
      // The fake mkdir uses the package NAME (npm would too); strip a version.
      const name = pkg.startsWith("@") ? pkg.replace(/(@[^/]+\/[^@]+)@.*/, "$1") : pkg.replace(/@.*/, "");
      const dir = join(packagesDir, "node_modules", name);
      mkdirSync(dir, { recursive: true });
      writeFileSync(join(dir, "package.json"), JSON.stringify({ name, autosk: { extensions: ["./index.ts"] } }), "utf8");
      writeFileSync(join(dir, "index.ts"), "export default function () {}\n", "utf8");
    }
    return { ok: true };
  };
  return { install, calls: () => calls };
}

/** Writes a single-file local extension and returns its absolute path. */
function writeLocalExt(dir: string, name: string): string {
  const p = join(dir, name);
  writeFileSync(p, "export default function () {}\n", "utf8");
  return p;
}

describe("installExtension — npm + local", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  test("npm: installs into packagesDir and upserts settings (installed:true)", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const packagesDir = join(home.path, ".autosk", "packages");
    const settingsPath = join(home.path, ".autosk", "settings.json");
    const { install, calls } = dirInstaller();

    const source = parseInstallSource("npm:@scope/pkg@1.2.3", { cwd: home.path, home: home.path });
    const res = await installExtension({ source, packagesDir, settingsPath, install });

    expect(res).toEqual({ installed: true, entry: "npm:@scope/pkg@1.2.3" });
    expect(calls()).toEqual([["@scope/pkg@1.2.3"]]); // versioned spec passed to npm
    expect(existsSync(join(packagesDir, "node_modules", "@scope", "pkg"))).toBe(true);
    expect(readSettingsExtensions(settingsPath)).toEqual(["npm:@scope/pkg@1.2.3"]);
  });

  test("npm: a failed install throws and does NOT write settings", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const packagesDir = join(home.path, ".autosk", "packages");
    const settingsPath = join(home.path, ".autosk", "settings.json");
    const install: BootstrapInstaller = async () => ({ ok: false, error: "npm boom" });

    const source = parseInstallSource("npm:@scope/pkg", { cwd: home.path, home: home.path });
    await expect(installExtension({ source, packagesDir, settingsPath, install })).rejects.toThrow(/npm boom/);
    expect(readSettingsExtensions(settingsPath)).toEqual([]);
  });

  test("local: existing path upserts settings without copying (installed:false)", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const packagesDir = join(home.path, ".autosk", "packages");
    const settingsPath = join(home.path, ".autosk", "settings.json");
    const extPath = writeLocalExt(home.path, "my-ext.ts");

    const source = parseInstallSource(extPath, { cwd: home.path, home: home.path });
    const res = await installExtension({ source, packagesDir, settingsPath });

    expect(res).toEqual({ installed: false, entry: extPath });
    expect(readSettingsExtensions(settingsPath)).toEqual([extPath]);
    // Not copied into the packages dir.
    expect(existsSync(packagesDir)).toBe(false);
  });

  test("npm: an EXPLICIT install is NOT gated by AUTOSK_NO_AUTO_INSTALL", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const packagesDir = join(home.path, ".autosk", "packages");
    const settingsPath = join(home.path, ".autosk", "settings.json");
    const { install, calls } = dirInstaller();
    process.env.AUTOSK_NO_AUTO_INSTALL = "1";
    try {
      const source = parseInstallSource("npm:@scope/pkg", { cwd: home.path, home: home.path });
      const res = await installExtension({ source, packagesDir, settingsPath, install });
      // The opt-out only disables the auto bootstrap/reconcile; an explicit
      // install still runs and writes settings.
      expect(res.installed).toBe(true);
      expect(calls()).toEqual([["@scope/pkg"]]);
      expect(readSettingsExtensions(settingsPath)).toEqual(["npm:@scope/pkg"]);
    } finally {
      delete process.env.AUTOSK_NO_AUTO_INSTALL;
    }
  });

  test("local: a missing path throws", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const packagesDir = join(home.path, ".autosk", "packages");
    const settingsPath = join(home.path, ".autosk", "settings.json");

    const source = parseInstallSource(join(home.path, "nope.ts"), { cwd: home.path, home: home.path });
    await expect(installExtension({ source, packagesDir, settingsPath })).rejects.toThrow(/not found/);
  });

  test("local: an existing but non-loadable path is rejected (not registered)", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const packagesDir = join(home.path, ".autosk", "packages");
    const settingsPath = join(home.path, ".autosk", "settings.json");
    // A real file that is NOT a .ts/.js extension file: fail fast at install time
    // rather than silently registering an entry that never loads.
    const notExt = join(home.path, "notes.txt");
    writeFileSync(notExt, "hello\n", "utf8");

    const source = parseInstallSource(notExt, { cwd: home.path, home: home.path });
    await expect(installExtension({ source, packagesDir, settingsPath })).rejects.toThrow(/not loadable/);
    // Nothing was written to settings.
    expect(readSettingsExtensions(settingsPath)).toEqual([]);
  });
});

describe("removeExtensionFromSettings", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  test("removes by name (any version), leaves node_modules untouched", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const packagesDir = join(home.path, ".autosk", "packages");
    const settingsPath = join(home.path, ".autosk", "settings.json");
    const { install } = dirInstaller();
    const source = parseInstallSource("npm:@scope/pkg@1.0.0", { cwd: home.path, home: home.path });
    await installExtension({ source, packagesDir, settingsPath, install });

    const removeSrc = parseInstallSource("npm:@scope/pkg", { cwd: home.path, home: home.path });
    // Reports the actual entry removed (the stored `@1.0.0` pin), not the
    // version-less argument it matched by name.
    expect(removeExtensionFromSettings({ source: removeSrc, settingsPath })).toEqual({
      removed: true,
      entries: ["npm:@scope/pkg@1.0.0"],
    });
    expect(readSettingsExtensions(settingsPath)).toEqual([]);
    // node_modules is left in place (like pi — settings.json only).
    expect(existsSync(join(packagesDir, "node_modules", "@scope", "pkg"))).toBe(true);

    // Idempotent: removing again reports removed:false.
    expect(removeExtensionFromSettings({ source: removeSrc, settingsPath })).toEqual({ removed: false, entries: [] });
  });
});

describe("listExtensionEntries — classification + resolved flag", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  test("lists global + project entries with kind and resolved", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    const { install } = dirInstaller();

    // Global: one resolvable npm install.
    await installExtension({
      source: parseInstallSource("npm:@g/flow", { cwd: home.path, home: home.path }),
      packagesDir: join(home.path, ".autosk", "packages"),
      settingsPath: join(home.path, ".autosk", "settings.json"),
      install,
    });
    // Project: a resolvable local file + a listed-but-unresolvable npm entry.
    const localExt = writeLocalExt(project.path, "p-ext.ts");
    await installExtension({
      source: parseInstallSource(localExt, { cwd: project.path, home: home.path }),
      packagesDir: join(project.path, ".autosk", "packages"),
      settingsPath: join(project.path, ".autosk", "settings.json"),
    });
    // Hand-write an uninstalled npm entry into project settings.
    writeFileSync(
      join(project.path, ".autosk", "settings.json"),
      JSON.stringify({ extensions: [localExt, "npm:@p/missing", "bogus"] }),
      "utf8",
    );

    const entries = listExtensionEntries({ projectRoot: project.path, home: home.path });

    expect(entries).toEqual([
      { source: "npm:@g/flow", scope: "global", kind: "npm", resolved: true },
      { source: localExt, scope: "project", kind: "local", resolved: true },
      { source: "npm:@p/missing", scope: "project", kind: "npm", resolved: false },
      { source: "bogus", scope: "project", kind: "invalid", resolved: false },
    ]);
  });
});

describe("ProjectManager — install / list / remove (global + -l)", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  function makePM(home: string, install: BootstrapInstaller): ProjectManager {
    return new ProjectManager({
      registry: new ProjectRegistry(`${home}/.autosk/projects.json`),
      store: { watch: false },
      extensions: { home },
      bootstrap: { install },
    });
  }

  test("global npm install writes ~/.autosk and project -l install writes the project", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    const { install } = dirInstaller();
    const pm = makePM(home.path, install);
    try {
      // Global install (no -l): does NOT need an open project.
      const g = await pm.installExtension(project.path, "npm:@g/flow@1.0.0", false);
      expect(g.scope).toBe("global");
      expect(g.installed).toBe(true);
      expect(g.settings_path).toBe(join(home.path, ".autosk", "settings.json"));
      expect(existsSync(join(home.path, ".autosk", "packages", "node_modules", "@g", "flow"))).toBe(true);
      expect(readSettingsExtensions(g.settings_path)).toEqual(["npm:@g/flow@1.0.0"]);

      // Project-local install (-l): into the project's packages dir + settings.
      // The project root is canonicalised (realpath) by the resolver.
      const projRoot = realpathSync(project.path);
      const projSettings = join(projRoot, ".autosk", "settings.json");
      const p = await pm.installExtension(project.path, "npm:@p/flow", true);
      expect(p.scope).toBe("project");
      expect(p.settings_path).toBe(projSettings);
      expect(existsSync(join(project.path, ".autosk", "packages", "node_modules", "@p", "flow"))).toBe(true);

      // list shows both scopes.
      const list = await pm.listExtensions(project.path);
      expect(list.entries).toContainEqual({ source: "npm:@g/flow@1.0.0", scope: "global", kind: "npm", resolved: true });
      expect(list.entries).toContainEqual({ source: "npm:@p/flow", scope: "project", kind: "npm", resolved: true });

      // remove (project scope) drops the project entry only.
      const r = await pm.removeExtension(project.path, "npm:@p/flow", true);
      expect(r).toEqual({
        scope: "project",
        source: "npm:@p/flow",
        settings_path: projSettings,
        removed: true,
      });
      expect(readSettingsExtensions(projSettings)).toEqual([]);
    } finally {
      await pm.close();
    }
  });

  test("local path install writes an absolute path into project settings (-l), not copied", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    const { install } = dirInstaller();
    const pm = makePM(home.path, install);
    try {
      const extPath = writeLocalExt(project.path, "local-ext.ts");
      // Pass a RELATIVE path; the daemon resolves it against cwd (the project).
      const res = await pm.installExtension(project.path, "./local-ext.ts", true);
      expect(res.scope).toBe("project");
      expect(res.installed).toBe(false);
      expect(res.source).toBe(extPath); // stored absolute
      expect(readSettingsExtensions(res.settings_path)).toEqual([extPath]);
    } finally {
      await pm.close();
    }
  });

  test("a -l install with no project at cwd is rejected (PROJECT_NOT_FOUND)", async () => {
    const home = tempDir();
    const bare = tempDir();
    cleanups.push(home.cleanup, bare.cleanup);
    const { install } = dirInstaller();
    const pm = makePM(home.path, install);
    try {
      await expect(pm.installExtension(bare.path, "npm:@x/y", true)).rejects.toThrow();
    } finally {
      await pm.close();
    }
  });

  test("an unrecognised source is rejected on install", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    const { install } = dirInstaller();
    const pm = makePM(home.path, install);
    try {
      await expect(pm.installExtension(project.path, "bare-name", false)).rejects.toThrow(/unrecognised extension source/);
    } finally {
      await pm.close();
    }
  });
});
