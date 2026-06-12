/**
 * Project manager (plan §3.7(1)): registry, walk-up resolution, lazy open,
 * init skeleton, and the "reads never auto-register" invariant.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdirSync, statSync, writeFileSync } from "node:fs";
import { stat } from "node:fs/promises";
import { join } from "node:path";

import {
  canonicalize,
  CapturingLogger,
  initProject,
  InvalidProjectError,
  ProjectManager,
  ProjectNotFoundError,
  ProjectRegistry,
  resolveProjectRoot,
} from "../src/index.ts";
import { tempDir } from "./helpers.ts";

describe("ProjectRegistry (~/.autosk/projects.json)", () => {
  let dir: ReturnType<typeof tempDir>;
  let regPath: string;

  beforeEach(() => {
    dir = tempDir();
    regPath = join(dir.path, "home", ".autosk", "projects.json");
  });
  afterEach(() => dir.cleanup());

  test("add / list / remove roundtrip, sorted by root", async () => {
    const reg = new ProjectRegistry(regPath);
    expect(await reg.list()).toEqual([]);

    await reg.add("/b/project", "beta");
    await reg.add("/a/project"); // name defaults to basename
    const list = await reg.list();
    expect(list).toEqual([
      { root: "/a/project", name: "project" },
      { root: "/b/project", name: "beta" },
    ]);

    // Re-adding the same root replaces (idempotent), never duplicates.
    await reg.add("/a/project", "renamed");
    expect((await reg.list()).filter((p) => p.root === "/a/project")).toEqual([
      { root: "/a/project", name: "renamed" },
    ]);

    expect(await reg.remove("/a/project")).toBe(true);
    expect(await reg.remove("/a/project")).toBe(false);
    expect(await reg.list()).toEqual([{ root: "/b/project", name: "beta" }]);
  });

  test("the file is 0600 and its directory 0700", async () => {
    const reg = new ProjectRegistry(regPath);
    await reg.add("/x/y", "y");
    expect(statSync(regPath).mode & 0o777).toBe(0o600);
    expect(statSync(join(dir.path, "home", ".autosk")).mode & 0o777).toBe(0o700);
  });

  test("a missing or empty file reads as an empty registry", async () => {
    expect(await new ProjectRegistry(regPath).list()).toEqual([]);
    mkdirSync(join(dir.path, "home2"), { recursive: true });
    const emptyPath = join(dir.path, "home2", "projects.json");
    writeFileSync(emptyPath, "   \n");
    expect(await new ProjectRegistry(emptyPath).list()).toEqual([]);
  });

  test("a non-empty corrupt file is NOT clobbered", async () => {
    mkdirSync(join(dir.path, "home3"), { recursive: true });
    const badPath = join(dir.path, "home3", "projects.json");
    writeFileSync(badPath, "{ this is not json");
    const reg = new ProjectRegistry(badPath);
    await expect(reg.list()).rejects.toThrow(/corrupt/);
    // The bad bytes are left on disk for the operator to rescue.
    expect(statSync(badPath).size).toBeGreaterThan(0);
  });

  test("survives reopen", async () => {
    await new ProjectRegistry(regPath).add("/x", "x");
    expect(await new ProjectRegistry(regPath).list()).toEqual([{ root: "/x", name: "x" }]);
  });
});

describe("resolveProjectRoot (walk-up)", () => {
  let dir: ReturnType<typeof tempDir>;

  beforeEach(() => dir = tempDir());
  afterEach(() => dir.cleanup());

  test("resolves from a nested subdirectory up to the nearest .autosk/", async () => {
    const root = join(dir.path, "proj");
    mkdirSync(join(root, ".autosk"), { recursive: true });
    const nested = join(root, "a", "b", "c");
    mkdirSync(nested, { recursive: true });

    const resolved = await resolveProjectRoot(nested);
    expect(resolved).toBe(await canonicalize(root));
  });

  test("an explicit override wins (and must contain .autosk/)", async () => {
    const root = join(dir.path, "proj");
    mkdirSync(join(root, ".autosk"), { recursive: true });
    expect(await resolveProjectRoot("/somewhere/else", root)).toBe(await canonicalize(root));
    await expect(resolveProjectRoot("/x", join(dir.path, "no-autosk"))).rejects.toThrow(
      ProjectNotFoundError,
    );
  });

  test("a relative cwd is rejected", async () => {
    await expect(resolveProjectRoot("relative/path")).rejects.toThrow(InvalidProjectError);
  });

  test("no .autosk/ anywhere up the tree throws ProjectNotFound", async () => {
    const lonely = join(dir.path, "lonely", "deep");
    mkdirSync(lonely, { recursive: true });
    await expect(resolveProjectRoot(lonely)).rejects.toThrow(ProjectNotFoundError);
  });
});

describe("initProject", () => {
  let dir: ReturnType<typeof tempDir>;
  beforeEach(() => dir = tempDir());
  afterEach(() => dir.cleanup());

  test("creates the tasks/ sessions/ extensions/ skeleton", async () => {
    const projDir = join(dir.path, "fresh");
    const info = await initProject(projDir);
    expect(info.root).toBe(await canonicalize(projDir));
    expect(info.name).toBe("fresh");
    for (const sub of ["tasks", "sessions", "extensions"]) {
      expect((await stat(join(projDir, ".autosk", sub))).isDirectory()).toBe(true);
    }
  });
});

describe("ProjectManager", () => {
  let dir: ReturnType<typeof tempDir>;
  let mgr: ProjectManager;
  let registry: ProjectRegistry;

  beforeEach(() => {
    dir = tempDir();
    registry = new ProjectRegistry(join(dir.path, "home", ".autosk", "projects.json"));
    // Point the extension loader at the test's temp home so open() never reads
    // the real ~/.autosk/ global extensions.
    mgr = new ProjectManager({
      registry,
      store: { watch: false },
      extensions: { home: join(dir.path, "home") },
      // A silent capturing logger keeps the suite output clean.
      logger: new CapturingLogger(),
    });
  });
  afterEach(async () => {
    await mgr.close();
    dir.cleanup();
  });

  test("resolve opens a project from a nested subdir without registering it", async () => {
    const root = join(dir.path, "proj");
    await initProject(root);
    const nested = join(root, "x", "y");
    mkdirSync(nested, { recursive: true });

    const handle = await mgr.resolve(nested);
    expect(handle.root).toBe(await canonicalize(root));
    // The store works (can create a task) ...
    const t = await handle.store.createTask({ title: "hi" });
    expect((await handle.store.taskView(t.id)).title).toBe("hi");

    // ... but a read must NOT auto-register the project.
    expect(await mgr.listProjects()).toEqual([]);
  });

  test("lazy open caches the handle (same instance for the same root)", async () => {
    const root = join(dir.path, "proj");
    await initProject(root);
    const nested = join(root, "sub", "dir");
    mkdirSync(nested, { recursive: true });

    const a = await mgr.resolve(root);
    const b = await mgr.resolve(nested); // walks up to the same root
    const c = await mgr.open(await canonicalize(root));
    expect(a).toBe(c);
    expect(b).toBe(a);
    expect(mgr.loaded()).toHaveLength(1);
  });

  test("concurrent first-resolve opens the project exactly once", async () => {
    const root = join(dir.path, "proj");
    await initProject(root);
    const handles = await Promise.all(Array.from({ length: 8 }, () => mgr.resolve(root)));
    expect(new Set(handles).size).toBe(1);
    expect(mgr.loaded()).toHaveLength(1);
  });

  test("addProject registers a canonical root; removeProject unregisters", async () => {
    const root = join(dir.path, "proj");
    await initProject(root);

    const added = await mgr.addProject(root);
    expect(added).toEqual({ root: await canonicalize(root), name: "proj" });
    expect(await mgr.listProjects()).toEqual([{ root: await canonicalize(root), name: "proj" }]);

    expect(await mgr.removeProject(root)).toBe(true);
    expect(await mgr.listProjects()).toEqual([]);
  });

  test("addProject walks up from a nested subdir, like resolve()", async () => {
    const root = join(dir.path, "proj");
    await initProject(root);
    const nested = join(root, "deep", "sub");
    mkdirSync(nested, { recursive: true });

    // add from a subdir resolves to the nearest .autosk/ above it.
    const added = await mgr.addProject(nested);
    expect(added).toEqual({ root: await canonicalize(root), name: "proj" });
    expect(await mgr.listProjects()).toEqual([{ root: await canonicalize(root), name: "proj" }]);

    // remove from a DIFFERENT subdir also resolves to the same root.
    const other = join(root, "x", "y");
    mkdirSync(other, { recursive: true });
    expect(await mgr.removeProject(other)).toBe(true);
    expect(await mgr.listProjects()).toEqual([]);
  });

  test("addProject refuses a directory without .autosk/", async () => {
    const bare = join(dir.path, "bare");
    mkdirSync(bare, { recursive: true });
    await expect(mgr.addProject(bare)).rejects.toThrow(ProjectNotFoundError);
  });
});
