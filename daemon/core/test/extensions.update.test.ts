/**
 * `extension.update` engine — `updateExtensions` (core) + the ProjectManager
 * scope selection (global / project / union).
 *
 * Everything uses an injected fake `install` (writes the package dir at the
 * "latest" version) and a fake `view` (returns registry versions from a map),
 * so the suite never touches the network.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import {
  ProjectManager,
  ProjectRegistry,
  initProject,
  parseInstallSource,
  updateExtensions,
  type BootstrapInstaller,
  type NpmViewVersion,
} from "../src/index.ts";
import { tempDir } from "./helpers.ts";

const autoskDir = (base: string) => join(base, ".autosk");
const packagesDir = (base: string) => join(autoskDir(base), "packages");
const settingsPath = (base: string) => join(autoskDir(base), "settings.json");

/** Writes a scope's `settings.json#extensions`. */
function writeSettings(base: string, extensions: string[]): void {
  mkdirSync(autoskDir(base), { recursive: true });
  writeFileSync(settingsPath(base), JSON.stringify({ extensions }), "utf8");
}

/** Seeds an installed package's `node_modules/<name>/package.json` version. */
function seedInstalled(base: string, name: string, version: string): void {
  const dir = join(packagesDir(base), "node_modules", name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "package.json"), JSON.stringify({ name, version }), "utf8");
}

/** Reads the currently-installed version under a scope (undefined if absent). */
function installedVersion(base: string, name: string): string | undefined {
  try {
    const pkg = JSON.parse(readFileSync(join(packagesDir(base), "node_modules", name, "package.json"), "utf8"));
    return pkg.version;
  } catch {
    return undefined;
  }
}

/** A fake registry: names in `versions` succeed; anything else fails (fail-open path). */
function fakeView(versions: Record<string, string>): { view: NpmViewVersion; calls: () => string[] } {
  const calls: string[] = [];
  const view: NpmViewVersion = async (name) => {
    calls.push(name);
    const v = versions[name];
    return v === undefined ? { ok: false, error: "not found" } : { ok: true, version: v };
  };
  return { view, calls: () => calls };
}

/** A fake installer: simulates `npm install <name>@latest` by writing the new version. */
function fakeInstall(
  latest: Record<string, string>,
  opts: { fail?: boolean } = {},
): { install: BootstrapInstaller; calls: () => { packagesDir: string; packages: string[] }[] } {
  const calls: { packagesDir: string; packages: string[] }[] = [];
  const install: BootstrapInstaller = async ({ packagesDir: dir, packages }) => {
    calls.push({ packagesDir: dir, packages: [...packages] });
    if (opts.fail) return { ok: false, error: "npm boom" };
    for (const spec of packages) {
      const name = spec.startsWith("@") ? spec.replace(/(@[^/]+\/[^@]+)@.*/, "$1") : spec.replace(/@.*/, "");
      const pkgDir = join(dir, "node_modules", name);
      mkdirSync(pkgDir, { recursive: true });
      writeFileSync(join(pkgDir, "package.json"), JSON.stringify({ name, version: latest[name] ?? "9.9.9" }), "utf8");
    }
    return { ok: true };
  };
  return { install, calls: () => calls };
}

describe("updateExtensions — core engine", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  test("floating npm: updated when latest != installed (single-scope batch install)", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    writeSettings(home.path, ["npm:@g/flow"]);
    seedInstalled(home.path, "@g/flow", "1.0.0");
    const { view, calls: viewCalls } = fakeView({ "@g/flow": "1.2.0" });
    const { install, calls } = fakeInstall({ "@g/flow": "1.2.0" });

    const res = await updateExtensions({ home: home.path, scopeFilter: "global", install, view });

    expect(res.dry_run).toBe(false);
    expect(res.changed).toBe(true);
    expect(res.entries).toContainEqual({
      source: "npm:@g/flow",
      name: "@g/flow",
      scope: "global",
      status: "updated",
      from_version: "1.0.0",
      to_version: "1.2.0",
    });
    expect(viewCalls()).toEqual(["@g/flow"]);
    expect(calls()).toEqual([{ packagesDir: packagesDir(home.path), packages: ["@g/flow@latest"] }]);
    expect(installedVersion(home.path, "@g/flow")).toBe("1.2.0");
  });

  test("up-to-date: no install runs, reported up-to-date", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    writeSettings(home.path, ["npm:@g/flow"]);
    seedInstalled(home.path, "@g/flow", "2.0.0");
    const { view } = fakeView({ "@g/flow": "2.0.0" });
    const { install, calls } = fakeInstall({});

    const res = await updateExtensions({ home: home.path, scopeFilter: "global", install, view });

    expect(res.changed).toBe(false);
    expect(res.entries).toContainEqual({
      source: "npm:@g/flow",
      name: "@g/flow",
      scope: "global",
      status: "up-to-date",
      from_version: "2.0.0",
      to_version: "2.0.0",
    });
    expect(calls()).toEqual([]); // nothing installed
  });

  test("version-pinned npm is skipped (never version-checked or installed)", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    writeSettings(home.path, ["npm:@g/flow@1.0.0"]);
    seedInstalled(home.path, "@g/flow", "1.0.0");
    const { view, calls: viewCalls } = fakeView({ "@g/flow": "2.0.0" }); // newer, but pinned
    const { install, calls } = fakeInstall({});

    const res = await updateExtensions({ home: home.path, scopeFilter: "global", install, view });

    expect(res.changed).toBe(false);
    expect(res.entries).toHaveLength(1);
    expect(res.entries[0]!.status).toBe("skipped");
    expect(res.entries[0]!.reason).toMatch(/version-pinned/);
    expect(viewCalls()).toEqual([]); // pinned entries are filtered before the registry check
    expect(calls()).toEqual([]);
  });

  test("local-path entry is skipped (loaded in place — nothing to update)", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    const localExt = join(home.path, "my-ext.ts");
    writeSettings(home.path, [localExt]);
    const { view } = fakeView({});
    const { install, calls } = fakeInstall({});

    const res = await updateExtensions({ home: home.path, scopeFilter: "global", install, view });

    expect(res.entries).toContainEqual({
      source: localExt,
      name: localExt,
      scope: "global",
      status: "skipped",
      reason: expect.stringMatching(/local/),
    });
    expect(calls()).toEqual([]);
  });

  test("per-scope batching: global + project each get one batched install (union)", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    writeSettings(home.path, ["npm:@g/a", "npm:@g/b"]);
    seedInstalled(home.path, "@g/a", "1.0.0");
    seedInstalled(home.path, "@g/b", "1.0.0");
    writeSettings(project.path, ["npm:@p/c"]);
    seedInstalled(project.path, "@p/c", "1.0.0");
    const { view } = fakeView({ "@g/a": "2.0.0", "@g/b": "2.0.0", "@p/c": "2.0.0" });
    const { install, calls } = fakeInstall({ "@g/a": "2.0.0", "@g/b": "2.0.0", "@p/c": "2.0.0" });

    const res = await updateExtensions({ home: home.path, projectRoot: project.path, install, view });

    expect(res.changed).toBe(true);
    const got = calls();
    expect(got).toHaveLength(2); // one batch per scope's packages dir
    expect(got).toContainEqual({ packagesDir: packagesDir(home.path), packages: ["@g/a@latest", "@g/b@latest"] });
    expect(got).toContainEqual({ packagesDir: packagesDir(project.path), packages: ["@p/c@latest"] });
    expect(res.entries.filter((e) => e.status === "updated")).toHaveLength(3);
  });

  test("scopeFilter selects scopes: global ignores project, project ignores global", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    writeSettings(home.path, ["npm:@g/a"]);
    seedInstalled(home.path, "@g/a", "1.0.0");
    writeSettings(project.path, ["npm:@p/c"]);
    seedInstalled(project.path, "@p/c", "1.0.0");
    const versions = { "@g/a": "2.0.0", "@p/c": "2.0.0" };
    const { view } = fakeView(versions);
    const { install } = fakeInstall(versions);

    // global: only @g/a considered.
    const g = await updateExtensions({
      home: home.path,
      projectRoot: project.path,
      scopeFilter: "global",
      view,
      install,
    });
    expect(g.entries.map((e) => e.name).sort()).toEqual(["@g/a"]);

    // project: only @p/c considered.
    const p = await updateExtensions({
      home: home.path,
      projectRoot: project.path,
      scopeFilter: "project",
      view,
      install,
    });
    expect(p.entries.map((e) => e.name).sort()).toEqual(["@p/c"]);

    // auto/union: both.
    const u = await updateExtensions({ home: home.path, projectRoot: project.path, view, install });
    expect(u.entries.map((e) => e.name).sort()).toEqual(["@g/a", "@p/c"]);
  });

  test("single-source filter updates only the matched package", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    writeSettings(home.path, ["npm:@g/a", "npm:@g/b"]);
    seedInstalled(home.path, "@g/a", "1.0.0");
    seedInstalled(home.path, "@g/b", "1.0.0");
    const { view } = fakeView({ "@g/a": "2.0.0", "@g/b": "2.0.0" });
    const { install, calls } = fakeInstall({ "@g/a": "2.0.0", "@g/b": "2.0.0" });
    const source = parseInstallSource("npm:@g/a", { cwd: home.path, home: home.path });

    const res = await updateExtensions({ home: home.path, scopeFilter: "global", source, install, view });

    expect(res.entries.map((e) => e.name)).toEqual(["@g/a"]);
    expect(calls()).toEqual([{ packagesDir: packagesDir(home.path), packages: ["@g/a@latest"] }]);
  });

  test("single-source that matches nothing throws a 'did you mean' error", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    writeSettings(home.path, ["npm:@g/a"]);
    seedInstalled(home.path, "@g/a", "1.0.0");
    const { view } = fakeView({ "@g/a": "2.0.0" });
    const { install } = fakeInstall({});
    const source = parseInstallSource("npm:@g/missing", { cwd: home.path, home: home.path });

    await expect(
      updateExtensions({ home: home.path, scopeFilter: "global", source, install, view }),
    ).rejects.toThrow(/did you mean `autosk ext add npm:@g\/missing`/);
  });

  test("dry-run reports available updates and installs nothing", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    writeSettings(home.path, ["npm:@g/a"]);
    seedInstalled(home.path, "@g/a", "1.0.0");
    const { view } = fakeView({ "@g/a": "2.0.0" });
    const { install, calls } = fakeInstall({ "@g/a": "2.0.0" });

    const res = await updateExtensions({ home: home.path, scopeFilter: "global", dryRun: true, install, view });

    expect(res.dry_run).toBe(true);
    expect(res.changed).toBe(false);
    expect(res.entries).toContainEqual({
      source: "npm:@g/a",
      name: "@g/a",
      scope: "global",
      status: "available",
      from_version: "1.0.0",
      to_version: "2.0.0",
    });
    expect(calls()).toEqual([]); // nothing installed
    expect(installedVersion(home.path, "@g/a")).toBe("1.0.0"); // untouched
  });

  test("fail-open: a registry lookup failure still updates (real run)", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    writeSettings(home.path, ["npm:@g/a"]);
    seedInstalled(home.path, "@g/a", "1.0.0");
    const { view } = fakeView({}); // every lookup fails
    const { install, calls } = fakeInstall({ "@g/a": "3.0.0" });

    const res = await updateExtensions({ home: home.path, scopeFilter: "global", install, view });

    expect(res.changed).toBe(true);
    expect(res.entries).toContainEqual({
      source: "npm:@g/a",
      name: "@g/a",
      scope: "global",
      status: "updated",
      from_version: "1.0.0",
      to_version: "3.0.0",
    });
    expect(calls()).toHaveLength(1);
  });

  test("fail-open: a registry lookup failure surfaces as 'unknown' in a dry-run", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    writeSettings(home.path, ["npm:@g/a"]);
    seedInstalled(home.path, "@g/a", "1.0.0");
    const { view } = fakeView({});
    const { install, calls } = fakeInstall({});

    const res = await updateExtensions({ home: home.path, scopeFilter: "global", dryRun: true, install, view });

    expect(res.entries).toContainEqual({
      source: "npm:@g/a",
      name: "@g/a",
      scope: "global",
      status: "unknown",
      from_version: "1.0.0",
    });
    expect(calls()).toEqual([]);
  });

  test("a failed install marks that scope's entries failed", async () => {
    const home = tempDir();
    cleanups.push(home.cleanup);
    writeSettings(home.path, ["npm:@g/a"]);
    seedInstalled(home.path, "@g/a", "1.0.0");
    const { view } = fakeView({ "@g/a": "2.0.0" });
    const { install } = fakeInstall({}, { fail: true });

    const res = await updateExtensions({ home: home.path, scopeFilter: "global", install, view });

    expect(res.changed).toBe(false);
    expect(res.entries).toHaveLength(1);
    expect(res.entries[0]!.status).toBe("failed");
    expect(res.entries[0]!.reason).toMatch(/npm boom/);
  });
});

describe("ProjectManager.updateExtensions — scope selection", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  function makePM(home: string, install: BootstrapInstaller, view: NpmViewVersion): ProjectManager {
    return new ProjectManager({
      registry: new ProjectRegistry(`${home}/.autosk/projects.json`),
      store: { watch: false },
      extensions: { home },
      bootstrap: { install, view },
    });
  }

  test("inside a project, no scope ⇒ union of global + project", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    writeSettings(home.path, ["npm:@g/x"]);
    seedInstalled(home.path, "@g/x", "1.0.0");
    writeSettings(project.path, ["npm:@p/y"]);
    seedInstalled(project.path, "@p/y", "1.0.0");
    const versions = { "@g/x": "2.0.0", "@p/y": "2.0.0" };
    const { view } = fakeView(versions);
    const { install } = fakeInstall(versions);
    const pm = makePM(home.path, install, view);
    try {
      const res = await pm.updateExtensions(project.path, {});
      const updated = res.entries.filter((e) => e.status === "updated");
      expect(updated.map((e) => e.scope).sort()).toEqual(["global", "project"]);
    } finally {
      await pm.close();
    }
  });

  test("--global forces global-only even inside a project", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    writeSettings(home.path, ["npm:@g/x"]);
    seedInstalled(home.path, "@g/x", "1.0.0");
    writeSettings(project.path, ["npm:@p/y"]);
    seedInstalled(project.path, "@p/y", "1.0.0");
    const versions = { "@g/x": "2.0.0", "@p/y": "2.0.0" };
    const { view } = fakeView(versions);
    const { install } = fakeInstall(versions);
    const pm = makePM(home.path, install, view);
    try {
      const res = await pm.updateExtensions(project.path, { scope: "global" });
      expect(res.entries.map((e) => e.scope)).toEqual(["global"]);
      expect(res.entries.map((e) => e.name)).toEqual(["@g/x"]);
    } finally {
      await pm.close();
    }
  });

  test("scope:project requires a project (PROJECT_NOT_FOUND outside one)", async () => {
    const home = tempDir();
    const bare = tempDir();
    cleanups.push(home.cleanup, bare.cleanup);
    const { view } = fakeView({});
    const { install } = fakeInstall({});
    const pm = makePM(home.path, install, view);
    try {
      await expect(pm.updateExtensions(bare.path, { scope: "project" })).rejects.toThrow();
    } finally {
      await pm.close();
    }
  });

  test("outside any project, auto ⇒ global only", async () => {
    const home = tempDir();
    const bare = tempDir();
    cleanups.push(home.cleanup, bare.cleanup);
    writeSettings(home.path, ["npm:@g/x"]);
    seedInstalled(home.path, "@g/x", "1.0.0");
    const versions = { "@g/x": "2.0.0" };
    const { view } = fakeView(versions);
    const { install } = fakeInstall(versions);
    const pm = makePM(home.path, install, view);
    try {
      const res = await pm.updateExtensions(bare.path, {});
      expect(res.entries.map((e) => e.scope)).toEqual(["global"]);
    } finally {
      await pm.close();
    }
  });

  test("a local-path source target is rejected", async () => {
    const home = tempDir();
    const project = tempDir();
    cleanups.push(home.cleanup, project.cleanup);
    await initProject(project.path);
    const { view } = fakeView({});
    const { install } = fakeInstall({});
    const pm = makePM(home.path, install, view);
    try {
      await expect(pm.updateExtensions(project.path, { source: "./my-ext.ts" })).rejects.toThrow(/local/);
    } finally {
      await pm.close();
    }
  });
});
