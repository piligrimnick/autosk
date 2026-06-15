/**
 * `@autosk/worktree` — the shipped `worktreeIsolation()` provider (plan §3.5).
 *
 * Ports v1's per-task git worktree isolation onto the v2
 * {@link IsolationProvider} contract. The engine
 * (`daemon/core/src/engine/session.ts`) calls `acquire` before scheduling an
 * isolated session (idempotently re-used across the run's steps) and `reap` on a
 * terminal transition (`done`/`cancel`). A worktree has nothing to "stop", so
 * this provider omits `release` entirely — keeping the dir across a step→step or
 * a human-park IS the absence of teardown. FAILURES ARE WRAPPED BY THE ENGINE
 * (`acquire` throw → `isolation_acquire_failed: <msg>`, `reap` throw →
 * `isolation_reap_failed: <msg>`), so this provider just throws descriptive
 * messages — it never parks or formats those prefixes itself.
 *
 * Deterministic mapping (byte-identical to the v1 Go/Rust slug so a worktree
 * allocated by either stack resolves to the same place):
 *
 * ```text
 * ~/.autosk/worktrees/<basename(canonRoot)>-<8hex(sha256(canonRoot))>/<task-id>
 * branch = autosk/<task-id>
 * ```
 */

import { createHash } from "node:crypto";
import { mkdirSync, realpathSync, rmSync, statSync } from "node:fs";
import { basename, dirname, isAbsolute, join, resolve } from "node:path";

import type { IsolationHandle, IsolationProvider, IsolationReapResult } from "@autosk/sdk";

/** Options for {@link worktreeIsolation}. */
export interface WorktreeIsolationOptions {
  /**
   * Home directory the worktree tree lives under (`<home>/.autosk/worktrees/…`).
   * Defaults to `process.env.HOME`. Injected by tests so they never touch the
   * operator's real `~/.autosk/`.
   */
  home?: string;
  /** `git` binary to shell out to. Defaults to `"git"`. */
  gitBin?: string;
}

/** The provider tag the registry/`workflow.get` renders for an isolated workflow. */
export const WORKTREE_TAG = "worktree";

/** Provider-internal bookkeeping carried on every {@link IsolationHandle}. */
interface WorktreeMeta extends Record<string, unknown> {
  branch: string;
  /** The canonical project root the worktree was checked out from. */
  projectRoot: string;
}

/**
 * Builds the shipped worktree {@link IsolationProvider}. Attach it to a workflow
 * via `isolation: worktreeIsolation()` (plan §3.6). `acquire` allocates (or
 * re-uses / re-allocates) the per-task worktree and hands the engine its path as
 * `ctx.cwd`; `reap` removes the dir on a terminal transition but PRESERVES the
 * `autosk/<task>` branch. There is no `release`: the dir is simply kept across
 * step→step and human-park (the env is reused on the next `acquire`).
 */
export function worktreeIsolation(opts: WorktreeIsolationOptions = {}): IsolationProvider {
  const gitBin = opts.gitBin ?? "git";

  return {
    tag: WORKTREE_TAG,

    async acquire({ projectRoot, taskId }): Promise<IsolationHandle> {
      const canon = canonRoot(projectRoot);
      const path = pathFor(canon, taskId, opts.home);
      const branch = branchFor(taskId);
      const meta: WorktreeMeta = { branch, projectRoot: canon };

      await ensureGitAvailable(gitBin);

      if (existsPath(path)) {
        // Present already (a prior step kept it, or an external pre-existing dir):
        // verify it is a healthy worktree OF THIS repo, else it is stranded.
        await verifyHealthy(gitBin, canon, path);
        return { cwd: path, meta };
      }

      // Missing dir → (re)allocate on the existing branch (v1 "missing worktree
      // auto-recovery"). A stale registration whose dir was rm'd out from under
      // git is cleared by `worktree prune` (and a force-remove fallback) before
      // the add, so the dir is genuinely recreated rather than reported existing.
      await ensureGitRepo(gitBin, canon);
      await pruneWorktrees(gitBin, canon);
      if (await worktreeRegisteredAt(gitBin, canon, path)) {
        await runGit(gitBin, canon, ["worktree", "remove", "--force", path]);
      }
      mkdirSync(dirname(path), { recursive: true });
      if (await branchExists(gitBin, canon, branch)) {
        const r = await runGit(gitBin, canon, ["worktree", "add", path, branch]);
        if (r.code !== 0) throw new Error(`worktree add (existing branch ${branch}): ${gitErr(r)}`);
      } else {
        const r = await runGit(gitBin, canon, ["worktree", "add", path, "-b", branch]);
        if (r.code !== 0) throw new Error(`worktree add (new branch ${branch}): ${gitErr(r)}`);
      }
      return { cwd: path, meta };
    },

    async reap({ projectRoot, taskId }, { force }): Promise<IsolationReapResult> {
      const canon = canonRoot(projectRoot);
      const path = pathFor(canon, taskId, opts.home);
      await ensureGitAvailable(gitBin);
      return cleanupTerminal(gitBin, canon, path, force);
    },
  };
}

// ---------------------------------------------------------------------------
// Deterministic path / branch derivation (byte-identical to v1).
// ---------------------------------------------------------------------------

/** Canonical branch name `autosk/<taskID>`. */
export function branchFor(taskId: string): string {
  return `autosk/${taskId}`;
}

/**
 * Absolute on-disk path for the `(projectRoot, taskID)` pair — the v1
 * `PathFor`. `projectRoot` is canonicalised (symlinks resolved, lexically
 * cleaned) so every caller computes the same slug.
 */
export function pathFor(projectRoot: string, taskId: string, home?: string): string {
  if (projectRoot.trim() === "") throw new Error("project root is empty");
  if (taskId.trim() === "") throw new Error("task id is empty");
  const h = home ?? process.env.HOME ?? "";
  if (h === "") throw new Error("user home dir not set");
  const canon = canonRoot(projectRoot);
  return join(h, ".autosk", "worktrees", slugFor(canon), taskId);
}

/** `basename(canon) + "-" + 8hex(sha256(canon))` — the v1 `slugFor`. */
export function slugFor(canon: string): string {
  const base = basename(canon);
  const digest = createHash("sha256").update(canon, "utf8").digest();
  const hex8 = digest.subarray(0, 4).toString("hex");
  return `${base}-${hex8}`;
}

/** Symlink-resolved, absolutised project root (v1 `canonRoot`). */
function canonRoot(projectRoot: string): string {
  const abs = resolve(projectRoot);
  try {
    return realpathSync(abs);
  } catch {
    return abs; // lexical-clean fallback when the path can't be canonicalised
  }
}

// ---------------------------------------------------------------------------
// git verbs.
// ---------------------------------------------------------------------------

/** Verifies the worktree at `path` is a healthy checkout of the repo at `canon`. */
async function verifyHealthy(gitBin: string, canon: string, path: string): Promise<void> {
  let wtGitdir: string;
  try {
    wtGitdir = await gitCommonDir(gitBin, path);
  } catch (e) {
    throw new Error(`worktree_stranded: ${path}: ${errMsg(e)}`);
  }
  let projGitdir: string;
  try {
    projGitdir = await gitCommonDir(gitBin, canon);
  } catch (e) {
    throw new Error(`not a git repository: ${canon}: ${errMsg(e)}`);
  }
  if (!sameDir(wtGitdir, projGitdir)) {
    throw new Error(`worktree_stranded: worktree gitdir=${wtGitdir}, project gitdir=${projGitdir}`);
  }
}

/**
 * The terminal-cleanup core behind {@link IsolationProvider.reap} (session-free,
 * keyed by identity).
 *
 * Removes the worktree dir while PRESERVING its branch, gated on `force`:
 * `force:false` leaves a dirty checkout in place and reports `{dirty:true}` so
 * the caller can warn; `force:true` removes it regardless. A vanished/absent dir
 * is a no-op (`{removed:false}`); a stranded dir that is not a healthy checkout
 * cannot be "dirty" in any recoverable sense and is reaped.
 */
async function cleanupTerminal(
  gitBin: string,
  canon: string,
  path: string,
  force: boolean,
): Promise<IsolationReapResult> {
  if (!existsPath(path)) {
    // Nothing on disk; best-effort clear of any stale git registration.
    if (canon !== "" && (await isGitRepo(gitBin, canon))) await pruneWorktrees(gitBin, canon);
    return { removed: false, dirty: false };
  }
  const { dirty, detail } = await worktreeDirty(gitBin, path);
  if (dirty && !force) return { removed: false, dirty: true, detail };
  await onTerminal(gitBin, canon, path);
  return { removed: true, dirty, detail: dirty ? detail : undefined };
}

/**
 * Reports whether the checkout at `path` has uncommitted changes (modified,
 * staged, OR untracked — anything `git status --porcelain` surfaces). A path
 * that is not a healthy worktree (status errors) reads as NOT dirty: there is no
 * recoverable working state to protect, so the caller may reap the stranded dir.
 */
async function worktreeDirty(gitBin: string, path: string): Promise<{ dirty: boolean; detail: string }> {
  const r = await runGit(gitBin, path, ["status", "--porcelain", "--untracked-files=all"]);
  if (r.code !== 0) return { dirty: false, detail: "" };
  const lines = r.stdout.split("\n").filter((l) => l.trim() !== "");
  if (lines.length === 0) return { dirty: false, detail: "" };
  return { dirty: true, detail: `${lines.length} uncommitted file(s)` };
}

/** Removes the worktree dir on a terminal transition while PRESERVING its branch. */
async function onTerminal(gitBin: string, canon: string, path: string): Promise<void> {
  // git itself broken / no project root → still try to reap the on-disk dir.
  if (canon === "" || !(await isGitRepo(gitBin, canon))) {
    if (existsPath(path)) rmSync(path, { recursive: true, force: true });
    return;
  }
  if (await worktreeRegisteredAt(gitBin, canon, path)) {
    const r = await runGit(gitBin, canon, ["worktree", "remove", "--force", path]);
    if (r.code !== 0) throw new Error(`worktree remove ${path}: ${gitErr(r)}`);
    return;
  }
  if (existsPath(path)) rmSync(path, { recursive: true, force: true });
  await runGit(gitBin, canon, ["worktree", "prune"]); // best-effort
}

/** Throws a descriptive `not a git repository` error if `canon` is not a repo. */
async function ensureGitRepo(gitBin: string, canon: string): Promise<void> {
  if (!(await isGitRepo(gitBin, canon))) {
    throw new Error(`not a git repository: ${canon}`);
  }
}

async function isGitRepo(gitBin: string, canon: string): Promise<boolean> {
  const r = await runGit(gitBin, canon, ["rev-parse", "--git-dir"]);
  return r.code === 0;
}

async function branchExists(gitBin: string, canon: string, branch: string): Promise<boolean> {
  const r = await runGit(gitBin, canon, ["show-ref", "--verify", "--quiet", `refs/heads/${branch}`]);
  if (r.code === 0) return true;
  if (r.code === 1) return false;
  throw new Error(`show-ref ${branch}: ${gitErr(r)}`);
}

async function worktreeRegisteredAt(gitBin: string, canon: string, target: string): Promise<boolean> {
  const r = await runGit(gitBin, canon, ["worktree", "list", "--porcelain"]);
  if (r.code !== 0) throw new Error(`worktree list: ${gitErr(r)}`);
  const canonTarget = canonRoot(target);
  for (const raw of r.stdout.split("\n")) {
    const line = raw.trim();
    if (line.startsWith("worktree ")) {
      const p = line.slice("worktree ".length).trim();
      if (sameDir(p, target) || sameDir(p, canonTarget)) return true;
    }
  }
  return false;
}

async function pruneWorktrees(gitBin: string, canon: string): Promise<void> {
  await runGit(gitBin, canon, ["worktree", "prune"]); // best-effort
}

/** `git -C cwd rev-parse --git-common-dir`, absolutised + canonicalised. */
async function gitCommonDir(gitBin: string, cwd: string): Promise<string> {
  const r = await runGit(gitBin, cwd, ["rev-parse", "--git-common-dir"]);
  if (r.code !== 0) throw new Error(`rev-parse --git-common-dir: ${gitErr(r)}`);
  const out = r.stdout.trim();
  if (out === "") throw new Error("rev-parse --git-common-dir: empty output");
  const abs = isAbsolute(out) ? out : join(cwd, out);
  try {
    return realpathSync(abs);
  } catch {
    return resolve(abs);
  }
}

/**
 * Caches a SUCCESSFUL `<gitBin> --version` per binary (a failure is re-checked,
 * never cached). Keyed by `gitBin` so a second provider built with a different
 * binary is validated independently instead of short-circuiting on the first.
 */
const gitAvailableByBin = new Map<string, boolean>();
async function ensureGitAvailable(gitBin: string): Promise<void> {
  if (gitAvailableByBin.get(gitBin) === true) return;
  let ok = false;
  try {
    const proc = Bun.spawn([gitBin, "--version"], { stdout: "ignore", stderr: "ignore", stdin: "ignore" });
    ok = (await proc.exited) === 0;
  } catch {
    ok = false;
  }
  if (ok) gitAvailableByBin.set(gitBin, true);
  else throw new Error(`git binary not found on PATH (${gitBin})`);
}

interface GitResult {
  code: number | null;
  stdout: string;
  stderr: string;
}

/** Runs `git -C <cwd> <args...>`, capturing stdout/stderr/exit code. */
async function runGit(gitBin: string, cwd: string, args: string[]): Promise<GitResult> {
  const proc = Bun.spawn([gitBin, "-C", cwd, ...args], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  return { code, stdout, stderr };
}

function gitErr(r: GitResult): string {
  const msg = `${r.stdout}${r.stderr}`.trim();
  return msg === "" ? `exit ${r.code}` : msg;
}

// ---------------------------------------------------------------------------
// small helpers.
// ---------------------------------------------------------------------------

function existsPath(p: string): boolean {
  // statSync follows symlinks (matches v1's os.Stat): a dangling symlink reads
  // as absent, not occupied.
  try {
    statSync(p);
    return true;
  } catch {
    return false;
  }
}

function sameDir(a: string, b: string): boolean {
  return resolve(a) === resolve(b);
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
