/**
 * Task + comment file IO with an mtime-keyed cache (plan §3.1, §3.7(2)).
 *
 * This is the low-level half of the store: it reads/writes `task.json` and
 * `comments.jsonl` and caches each by its {@link fileSig file signature}. It is
 * deliberately **lock-free and policy-free** — the owning {@link Store}
 * serialises every task operation (mutations AND reconciliation) under one
 * per-task lock, so this class never has to reason about concurrency or
 * hybrid-ownership. The cached `StoredTask` is the engine's authoritative view;
 * reconciliation compares it against disk and decides accept vs reject.
 */

import { readFile, readdir } from "node:fs/promises";

import type { Comment } from "@autosk/sdk";
import { atomicWrite, statSig } from "./atomic.ts";
import { ProjectPaths } from "./paths.ts";
import {
  parseCommentsLenient,
  parseTask,
  serializeComments,
  serializeTask,
  type StoredTask,
} from "./records.ts";

/** A cached file: its signature plus the parsed value. */
interface Cached<T> {
  sig: string;
  value: T;
}

export class TaskStore {
  private readonly paths: ProjectPaths;

  /** Engine-authoritative task cache, keyed by task id. */
  private taskCache = new Map<string, Cached<StoredTask>>();
  /**
   * Comment cache, keyed by task id (comments have no engine ownership). Stores
   * the parsed comments plus how many malformed lines the lenient parser skipped
   * (C5), so the Store can surface a silently-dropped human comment.
   */
  private commentCache = new Map<string, { sig: string; comments: Comment[]; skipped: number }>();

  constructor(paths: ProjectPaths) {
    this.paths = paths;
  }

  // -- cache accessors -----------------------------------------------------

  /** The engine-authoritative task, if cached. */
  peek(id: string): StoredTask | undefined {
    return this.taskCache.get(id)?.value;
  }

  /** The cached file signature for a task, if any. */
  peekSig(id: string): string | undefined {
    return this.taskCache.get(id)?.sig;
  }

  /** Seeds/overwrites the cache without touching disk (accept an external edit). */
  setCache(id: string, sig: string, task: StoredTask): void {
    this.taskCache.set(id, { sig, value: task });
  }

  /** Forgets a task (external deletion accepted). */
  dropCache(id: string): void {
    this.taskCache.delete(id);
    this.commentCache.delete(id);
  }

  /** Currently-cached task ids. */
  cachedIds(): string[] {
    return [...this.taskCache.keys()];
  }

  /** Every task id with a `task.json` on disk. */
  async listIdsOnDisk(): Promise<string[]> {
    let entries: string[];
    try {
      entries = await readdir(this.paths.tasksDir);
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code === "ENOENT") return [];
      throw e;
    }
    const ids: string[] = [];
    for (const name of entries) {
      if (name.startsWith(".")) continue;
      const probe = await statSig(this.paths.taskJson(name));
      if (probe) ids.push(name);
    }
    return ids.sort();
  }

  // -- task IO -------------------------------------------------------------

  /**
   * Cheap signature probe for a `task.json` WITHOUT reading or parsing its bytes
   * — the reconciler's steady-state no-op short-circuit (the mtime-keyed cache,
   * plan §3.7(2)). `null` when no `task.json` exists. When the returned sig
   * equals the cached engine sig, the file is unchanged and the reconciler skips
   * the `readDisk` entirely, keeping `listTaskViews` O(N) stats instead of
   * O(N) reads+parses.
   */
  async probeSig(id: string): Promise<string | null> {
    const probe = await statSig(this.paths.taskJson(id));
    return probe ? probe.sig : null;
  }

  /**
   * Reads a task straight off disk (bypassing the cache) — the reconciler's
   * window into the external file state. `null` when no `task.json` exists.
   */
  async readDisk(id: string): Promise<{ sig: string; task: StoredTask } | null> {
    const probe = await statSig(this.paths.taskJson(id));
    if (!probe) return null;
    const text = await readFile(this.paths.taskJson(id), "utf8");
    const task = parseTask(text);
    // The directory name is the canonical id; a human-edited `id` field inside
    // the file must never override it (else a restore/accept would write engine
    // state to a phantom `tasks/<file-id>/` dir, C2). Force the dir id — the
    // file's id field is redundant and self-heals on the next write.
    task.id = id;
    return { sig: probe.sig, task };
  }

  /**
   * Reads a task through the cache (refreshing on signature change). Used by
   * mutations to fetch the base record. `null` when no `task.json` exists.
   */
  async read(id: string): Promise<StoredTask | null> {
    const probe = await statSig(this.paths.taskJson(id));
    if (!probe) {
      this.taskCache.delete(id);
      return null;
    }
    const cached = this.taskCache.get(id);
    if (cached && cached.sig === probe.sig) return cached.value;
    const text = await readFile(this.paths.taskJson(id), "utf8");
    const task = parseTask(text);
    task.id = id; // directory is authoritative over the file's id field (C2).
    this.taskCache.set(id, { sig: probe.sig, value: task });
    return task;
  }

  /** Writes a full record, updates the cache, and returns the new signature. */
  async writeTask(task: StoredTask): Promise<string> {
    const sig = await atomicWrite(this.paths.taskJson(task.id), serializeTask(task));
    this.taskCache.set(task.id, { sig, value: task });
    return sig;
  }

  // -- comment IO (always disk-as-truth; no engine ownership) --------------

  /**
   * Refreshes the comment cache for `id` and returns the CANONICAL cached entry
   * (never clone its `comments` for the caller — see {@link readComments}).
   * `null` when no `comments.jsonl` exists. Shared by `readComments` (which
   * clones) and `commentStats` (which only needs the count + skip signal).
   */
  private async refreshComments(
    id: string,
  ): Promise<{ sig: string; comments: Comment[]; skipped: number } | null> {
    const probe = await statSig(this.paths.commentsJsonl(id));
    if (!probe) {
      this.commentCache.delete(id);
      return null;
    }
    const cached = this.commentCache.get(id);
    if (cached && cached.sig === probe.sig) return cached;
    const text = await readFile(this.paths.commentsJsonl(id), "utf8");
    const { comments, skipped } = parseCommentsLenient(text);
    const entry = { sig: probe.sig, comments, skipped };
    this.commentCache.set(id, entry);
    return entry;
  }

  /**
   * Reads a task's comments, refreshing the cache when the file changed.
   * Returns CLONES so a caller that mutates a comment (edit/delete builds the
   * next list in place) cannot corrupt the shared cache.
   */
  async readComments(id: string): Promise<Comment[]> {
    const r = await this.refreshComments(id);
    return r ? r.comments.map((c) => ({ ...c })) : [];
  }

  /**
   * Comment count for `TaskView.comment_count`, plus how many malformed lines
   * the lenient parser skipped and the file signature. Cheap: peeks the cache by
   * signature and returns `.length` WITHOUT cloning every comment. The `skipped`
   * count + `sig` let the Store warn once per signature about a silently-dropped
   * human comment (C5); `sig` is null when no `comments.jsonl` exists.
   */
  async commentStats(id: string): Promise<{ count: number; skipped: number; sig: string | null }> {
    const r = await this.refreshComments(id);
    if (!r) return { count: 0, skipped: 0, sig: null };
    return { count: r.comments.length, skipped: r.skipped, sig: r.sig };
  }

  /** Writes the whole comment list atomically and updates the cache. */
  async writeComments(id: string, comments: Comment[]): Promise<void> {
    const sig = await atomicWrite(this.paths.commentsJsonl(id), serializeComments(comments));
    this.commentCache.set(id, { sig, comments: comments.map((c) => ({ ...c })), skipped: 0 });
  }
}
