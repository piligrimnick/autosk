/**
 * Real git-backed worktree provider tests (P6 acceptance #1): the acquire/release
 * matrix, branch preservation on terminal, dir-kept on human-park/sibling,
 * missing-dir re-allocation, and the throw paths (stranded dir, non-git root).
 *
 * The slug formula is cross-checked against the v1 derivation (basename-8hex of
 * sha256(canonRoot)). Each test uses a hermetic temp `home` (passed explicitly,
 * never via `process.env.HOME`) so parallel test files never collide.
 */

import { createHash } from "node:crypto";
import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, join } from "node:path";

import { branchFor, pathFor, slugFor, worktreeIsolation } from "../src/index.ts";

let gitOk = false;
beforeAll(() => {
  const p = Bun.spawnSync(["git", "--version"]);
  gitOk = p.exitCode === 0;
});

const temps: string[] = [];
afterEach(() => {
  for (const d of temps.splice(0)) rmSync(d, { recursive: true, force: true });
});

function mkTemp(prefix: string): string {
  const d = realpathSync(mkdtempSync(join(tmpdir(), prefix)));
  temps.push(d);
  return d;
}

function git(args: string[], cwd: string): { code: number; stdout: string; stderr: string } {
  const p = Bun.spawnSync(["git", ...args], {
    cwd,
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "t",
      GIT_AUTHOR_EMAIL: "t@e",
      GIT_COMMITTER_NAME: "t",
      GIT_COMMITTER_EMAIL: "t@e",
    },
  });
  return { code: p.exitCode, stdout: p.stdout.toString(), stderr: p.stderr.toString() };
}

/** A fresh git repo with one commit; returns its (realpath'd) root. */
function makeRepo(): string {
  const root = mkTemp("wt-repo-");
  git(["init", "-q", "-b", "main"], root);
  Bun.spawnSync(["sh", "-c", `printf x > '${join(root, "README")}'`]);
  git(["add", "."], root);
  git(["commit", "-q", "-m", "init"], root);
  return root;
}

function branchListed(root: string, branch: string): boolean {
  return git(["show-ref", "--verify", "--quiet", `refs/heads/${branch}`], root).code === 0;
}

describe("worktreeIsolation — path/branch derivation", () => {
  test("branchFor is autosk/<task>", () => {
    expect(branchFor("ask-deadbe")).toBe("autosk/ask-deadbe");
  });

  test("slugFor is basename-8hex(sha256(canonRoot)) — byte-identical to v1", () => {
    const canon = "/tmp/myproj";
    const want = `${basename(canon)}-${createHash("sha256").update(canon, "utf8").digest().subarray(0, 4).toString("hex")}`;
    expect(slugFor(canon)).toBe(want);
    expect(slugFor(canon).startsWith("myproj-")).toBe(true);
    expect(slugFor(canon).length).toBe("myproj-".length + 8);
  });

  test("pathFor lands under <home>/.autosk/worktrees/<slug>/<task>", () => {
    const home = "/home/u";
    // A real dir so the provider's realpath canonicalisation is deterministic.
    const root = mkTemp("wt-pf-");
    const canon = realpathSync(root);
    const p = pathFor(root, "ask-abc123", home);
    expect(p).toBe(join(home, ".autosk", "worktrees", slugFor(canon), "ask-abc123"));
  });

  test("pathFor rejects empty root / task / home", () => {
    expect(() => pathFor("", "ask-1", "/home/u")).toThrow(/project root is empty/);
    expect(() => pathFor("/tmp/p", "", "/home/u")).toThrow(/task id is empty/);
    expect(() => pathFor("/tmp/p", "ask-1", "")).toThrow(/home dir not set/);
  });
});

describe("worktreeIsolation — acquire/release matrix (real git)", () => {
  test("acquire allocates dir on a fresh branch; terminal release removes dir, preserves branch", async () => {
    if (!gitOk) return;
    const home = mkTemp("wt-home-");
    const root = makeRepo();
    const prov = worktreeIsolation({ home });
    const taskId = "ask-deadbe";

    const handle = await prov.acquire({ projectRoot: root, taskId });
    expect(handle.cwd).toBe(pathFor(root, taskId, home));
    expect(existsSync(handle.cwd)).toBe(true);
    expect(handle.meta?.branch).toBe("autosk/ask-deadbe");
    expect(branchListed(root, "autosk/ask-deadbe")).toBe(true);

    await prov.release(handle, { terminal: true, force: true });
    expect(existsSync(handle.cwd)).toBe(false); // dir removed
    expect(branchListed(root, "autosk/ask-deadbe")).toBe(true); // branch PRESERVED
  });

  test("non-terminal release keeps the dir (human-park / sibling step) and re-acquire reuses it", async () => {
    if (!gitOk) return;
    const home = mkTemp("wt-home-");
    const root = makeRepo();
    const prov = worktreeIsolation({ home });
    const taskId = "ask-keepit";

    const h1 = await prov.acquire({ projectRoot: root, taskId });
    await prov.release(h1, { terminal: false, force: false });
    expect(existsSync(h1.cwd)).toBe(true); // dir kept

    // Re-acquire (next sibling step): same path, still healthy, dir untouched.
    const h2 = await prov.acquire({ projectRoot: root, taskId });
    expect(h2.cwd).toBe(h1.cwd);
    expect(existsSync(h2.cwd)).toBe(true);

    await prov.release(h2, { terminal: true, force: true });
    expect(existsSync(h2.cwd)).toBe(false);
  });

  test("missing-dir re-allocation: a vanished dir is recreated on the existing branch", async () => {
    if (!gitOk) return;
    const home = mkTemp("wt-home-");
    const root = makeRepo();
    const prov = worktreeIsolation({ home });
    const taskId = "ask-vanish";

    const h1 = await prov.acquire({ projectRoot: root, taskId });
    expect(existsSync(h1.cwd)).toBe(true);
    // rm -rf the worktree dir out from under git (leaves a stale registration).
    rmSync(h1.cwd, { recursive: true, force: true });
    expect(existsSync(h1.cwd)).toBe(false);

    const h2 = await prov.acquire({ projectRoot: root, taskId });
    expect(h2.cwd).toBe(h1.cwd);
    expect(existsSync(h2.cwd)).toBe(true); // re-allocated
    expect(branchListed(root, "autosk/ask-vanish")).toBe(true);
  });
});

describe("worktreeIsolation — reap (session-free cleanup) + dirty handling", () => {
  test("reap removes a clean worktree, preserves the branch", async () => {
    if (!gitOk) return;
    const home = mkTemp("wt-home-");
    const root = makeRepo();
    const prov = worktreeIsolation({ home });
    const taskId = "ask-clean1";

    const handle = await prov.acquire({ projectRoot: root, taskId });
    expect(existsSync(handle.cwd)).toBe(true);

    // No live handle is passed — reap re-derives the path from identity.
    const r = await prov.reap!({ projectRoot: root, taskId }, { force: false });
    expect(r).toEqual({ removed: true, dirty: false, detail: undefined });
    expect(existsSync(handle.cwd)).toBe(false); // dir removed
    expect(branchListed(root, "autosk/ask-clean1")).toBe(true); // branch PRESERVED
  });

  test("reap on a missing worktree is a no-op", async () => {
    if (!gitOk) return;
    const home = mkTemp("wt-home-");
    const root = makeRepo();
    const prov = worktreeIsolation({ home });

    const r = await prov.reap!({ projectRoot: root, taskId: "ask-never" }, { force: false });
    expect(r).toEqual({ removed: false, dirty: false });
  });

  test("reap without force REFUSES a dirty worktree; force removes it (branch kept)", async () => {
    if (!gitOk) return;
    const home = mkTemp("wt-home-");
    const root = makeRepo();
    const prov = worktreeIsolation({ home });
    const taskId = "ask-dirty1";

    const handle = await prov.acquire({ projectRoot: root, taskId });
    // An UNTRACKED file makes the checkout dirty (status --porcelain shows `??`).
    writeFileSync(join(handle.cwd, "scratch.txt"), "uncommitted");

    const refused = await prov.reap!({ projectRoot: root, taskId }, { force: false });
    expect(refused.removed).toBe(false);
    expect(refused.dirty).toBe(true);
    expect(refused.detail).toMatch(/uncommitted file/);
    expect(existsSync(handle.cwd)).toBe(true); // LEFT IN PLACE

    const forced = await prov.reap!({ projectRoot: root, taskId }, { force: true });
    expect(forced.removed).toBe(true);
    expect(forced.dirty).toBe(true);
    expect(existsSync(handle.cwd)).toBe(false); // removed
    expect(branchListed(root, "autosk/ask-dirty1")).toBe(true); // branch PRESERVED
  });

  test("release({terminal,force:false}) throws on a dirty worktree; force:true removes it", async () => {
    if (!gitOk) return;
    const home = mkTemp("wt-home-");
    const root = makeRepo();
    const prov = worktreeIsolation({ home });
    const taskId = "ask-dirty2";

    const handle = await prov.acquire({ projectRoot: root, taskId });
    writeFileSync(join(handle.cwd, "scratch.txt"), "uncommitted");

    await expect(prov.release(handle, { terminal: true, force: false })).rejects.toThrow(/worktree_dirty/);
    expect(existsSync(handle.cwd)).toBe(true); // not removed

    await prov.release(handle, { terminal: true, force: true });
    expect(existsSync(handle.cwd)).toBe(false); // removed
    expect(branchListed(root, "autosk/ask-dirty2")).toBe(true);
  });
});

describe("worktreeIsolation — throw paths", () => {
  test("non-git root → acquire throws (engine wraps as isolation_acquire_failed)", async () => {
    if (!gitOk) return;
    const home = mkTemp("wt-home-");
    const notRepo = mkTemp("wt-norepo-"); // a plain dir, never `git init`'d
    const prov = worktreeIsolation({ home });
    await expect(prov.acquire({ projectRoot: notRepo, taskId: "ask-nogit" })).rejects.toThrow(
      /not a git repository/,
    );
  });

  test("stranded dir (foreign repo at the worktree path) → acquire throws", async () => {
    if (!gitOk) return;
    const home = mkTemp("wt-home-");
    const root = makeRepo();
    const prov = worktreeIsolation({ home });
    const taskId = "ask-strand";

    // Pre-create an UNRELATED git repo exactly where this task's worktree would go.
    const path = pathFor(root, taskId, home);
    Bun.spawnSync(["mkdir", "-p", path]);
    git(["init", "-q"], path);

    await expect(prov.acquire({ projectRoot: root, taskId })).rejects.toThrow(/worktree_stranded/);
  });
});
