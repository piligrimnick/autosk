/**
 * The per-project file store (plan §3.7(2)) — the front door over `TaskStore`
 * + `SessionStore`.
 *
 * Responsibilities:
 *  - **Mutations** (create/update/enroll-position/block/comment/session): each
 *    serialised per task id under one `KeyedMutex`, written atomically.
 *  - **Reconciliation** (hybrid ownership): an external edit to `title` /
 *    `description` / `blocked_by` is accepted; an external edit to
 *    `status` / `step` / `workflow` of a task with a LIVE session is rejected —
 *    the engine-owned fields are restored from the cached engine state and a
 *    warning is logged. Echo suppression is implicit: the daemon's own write
 *    leaves the cached signature equal to the file's, so the watcher event that
 *    follows reconciles to a no-op. Reconciliation runs under the SAME per-task
 *    lock as mutations, so it can never race a legitimate engine write.
 *  - **Derived views**: `blocked` / `blocks` from `blocked_by` edges across the
 *    project, plus `comment_count`.
 *  - **Watcher wiring**: `fs.watch(.autosk/tasks)` + a slow safety rescan, both
 *    funnelling into reconciliation.
 */

import { mkdir } from "node:fs/promises";

import {
  newCommentId,
  newTaskId,
  type Comment,
  type TaskFilter,
  type TaskRef,
  type TaskStatus,
  type TaskView,
} from "@autosk/sdk";

import { systemClock, type Clock } from "./clock.ts";
import { KeyedMutex } from "./lock.ts";
import { consoleLogger, type Logger } from "./logger.ts";
import { ProjectPaths } from "./paths.ts";
import type { StoredTask } from "./records.ts";
import { SessionStore } from "./sessionStore.ts";
import { TaskStore } from "./taskStore.ts";
import { TasksWatcher, type WatcherOptions } from "./watcher.ts";

/** Statuses that count as an *open* blocker (anything not terminal). */
const OPEN_STATUSES: ReadonlySet<TaskStatus> = new Set(["new", "work", "human"]);

/**
 * The minimal task projection the scheduler needs (plan §3.7(3)). Deliberately
 * lighter than {@link TaskView}: NO `comment_count` (a per-task `comments.jsonl`
 * read) and NO reverse `blocks` index — the scheduler only consults
 * id/status/workflow/step/blocked, and it runs on every session completion, so
 * the hot path must not pay for those.
 */
export interface SchedulingRow {
  id: string;
  status: TaskStatus;
  workflow: string | null;
  step: string | null;
  blocked: boolean;
}

export interface StoreOptions {
  clock?: Clock;
  logger?: Logger;
  /** Watcher config, or `false` to run without a live watcher (most tests). */
  watch?: WatcherOptions | false;
}

export class Store {
  readonly paths: ProjectPaths;
  readonly sessions: SessionStore;

  private readonly tasks: TaskStore;
  private readonly clock: Clock;
  private readonly logger: Logger;
  private readonly locks = new KeyedMutex();
  private readonly watchOpts: WatcherOptions | false;

  private watcher: TasksWatcher | null = null;
  private opened = false;

  /**
   * Last-warned signature per persistently-corrupt `task.json`, so a broken idle
   * file warns once per signature change rather than once per reconcile (M2) — a
   * polling `task.list` must not spin the daemon log on a file the operator has
   * not fixed yet. Cleared once the file parses cleanly (or disappears).
   */
  private readonly corruptWarned = new Map<string, string>();

  /**
   * Last-warned signature per `comments.jsonl` that dropped a malformed line, so
   * a silently-skipped human comment warns once per signature rather than once
   * per list (C5). Same de-dupe shape as {@link corruptWarned} (M2); cleared
   * once the file parses cleanly (or disappears).
   */
  private readonly commentSkipWarned = new Map<string, string>();

  constructor(root: string, opts: StoreOptions = {}) {
    this.paths = new ProjectPaths(root);
    this.tasks = new TaskStore(this.paths);
    this.sessions = new SessionStore(this.paths, this.locks, opts.logger ?? consoleLogger);
    this.clock = opts.clock ?? systemClock;
    this.logger = opts.logger ?? consoleLogger;
    this.watchOpts = opts.watch ?? {};
  }

  /** Project root (parent of `.autosk/`). */
  get root(): string {
    return this.paths.root;
  }

  // -- lifecycle -----------------------------------------------------------

  /** Startup scan (sessions then tasks) + watcher start. Idempotent. */
  async open(): Promise<void> {
    if (this.opened) return;
    try {
      // Ensure the watched dirs exist so the recursive watcher attaches even on
      // a project opened before its first write (the scans below tolerate absence).
      await mkdir(this.paths.tasksDir, { recursive: true });
      await mkdir(this.paths.sessionsDir, { recursive: true });

      // Sessions first, so `hasLiveSession` is accurate while tasks load.
      const sessionScan = await this.sessions.scan();
      for (const err of sessionScan.errors) {
        this.logger.warn(`scan: skipped malformed session ${err.id}: ${err.error}`);
      }

      // Load every task on disk into the cache (disk is truth at startup). One
      // corrupt task.json must never brick the whole open(), so each task
      // reconciles defensively (parse failures are logged + skipped, not thrown).
      for (const id of await this.tasks.listIdsOnDisk()) {
        await this.safeReconcile(id);
      }

      if (this.watchOpts !== false) {
        this.watcher = new TasksWatcher(
          this.paths.tasksDir,
          {
            onTaskTouched: (id) => {
              void this.reconcileTask(id).catch((e) =>
                this.logger.error(`reconcile ${id}: ${e instanceof Error ? e.message : String(e)}`),
              );
            },
            onRescan: () => {
              void this.reconcileAll().catch((e) =>
                this.logger.error(`rescan: ${e instanceof Error ? e.message : String(e)}`),
              );
            },
          },
          this.watchOpts,
        );
        this.watcher.start();
      }

      // Mark opened only after a clean startup; on any failure leave it false so
      // a retry re-runs from scratch instead of early-returning half-initialised.
      this.opened = true;
    } catch (e) {
      this.watcher?.close();
      this.watcher = null;
      throw e;
    }
  }

  /** Stops the watcher. Idempotent. */
  async close(): Promise<void> {
    this.watcher?.close();
    this.watcher = null;
    this.opened = false;
  }

  // -- reconciliation ------------------------------------------------------

  /** Reconciles one task (acquires the per-task lock). */
  async reconcileTask(id: string): Promise<void> {
    await this.locks.run(id, () => this.reconcileLocked(id));
  }

  /**
   * Reconcile one task, swallowing + logging any unexpected error. Used by the
   * bulk paths (startup scan, `reconcileAll`, the watcher rescan) so a single
   * task failing can never abort the whole sweep (mirrors the watcher's
   * per-event `.catch`). Parse failures are already handled inside
   * `reconcileLocked`; this is the belt-and-braces net for anything else.
   */
  private async safeReconcile(id: string): Promise<void> {
    try {
      await this.locks.run(id, () => this.reconcileLocked(id));
    } catch (e) {
      this.logger.error(`reconcile ${id}: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  /**
   * Reconciles every task currently on disk or cached.
   *
   * `listIdsOnDisk` stats each `task.json` once (existence), and
   * `reconcileLocked` re-probes the signature once more UNDER the per-task lock
   * (C4). The second stat is deliberate, not redundant: a signature taken at
   * listing time can go stale before the lock is acquired, so an edit landing in
   * that window would be MISSED if we trusted the listing signature. Re-probing
   * under the lock is what lets reconcile observe an edit that arrives between
   * listing and lock — the extra stat buys that freshness. The B2 fix already
   * removed the O(N) read+parse that was the real cost; the residual stat is
   * cheap and load-bearing.
   */
  async reconcileAll(): Promise<void> {
    const ids = new Set<string>(await this.tasks.listIdsOnDisk());
    for (const id of this.tasks.cachedIds()) ids.add(id);
    for (const id of ids) {
      await this.safeReconcile(id);
    }
  }

  /** Forgets the last-warned corrupt signature for a task (M2 de-dupe memo). */
  private clearCorruptMemo(id: string): void {
    this.corruptWarned.delete(id);
  }

  /**
   * Caller MUST hold the per-task lock for `id`.
   *
   * Ownership is keyed on a **live session** (`hasLiveSession`), not on mere
   * enrolment: an enrolled task with no live session (e.g. a parked
   * `status:"human"` step, or the gap between sessions) ACCEPTS external
   * status/step/workflow edits by design. This matches the plan's literal rule
   * ("...of a task WITH A LIVE SESSION are rejected") and the acceptance test;
   * P4's scheduler treats the live-session window as the engine-owned window.
   */
  private async reconcileLocked(id: string): Promise<void> {
    // Probe the file signature BEFORE reading the bytes. When it equals the
    // cached engine sig the file is unchanged — our own write (echo) or no
    // change at all — so we skip the readFile+parse entirely. That is the whole
    // point of the mtime-keyed cache (plan §3.7(2)): steady-state
    // `listTaskViews` over an unchanged project stays O(N) cheap stats, not
    // O(N) reads+parses (B2). A real external edit bumps the signature
    // (mtime/ctime/size/ino), which still drops us into the full read below.
    const sig = await this.tasks.probeSig(id);
    const engine = this.tasks.peek(id);
    const engineSig = this.tasks.peekSig(id);

    if (sig === null) {
      // The task.json is gone.
      this.clearCorruptMemo(id);
      if (!engine) return; // unknown task, gone — nothing to do
      if (this.sessions.hasLiveSession(id)) {
        this.logger.warn(
          `reconcile: ${id} deleted externally while a session is live; restoring from engine state`,
        );
        await this.tasks.writeTask(engine);
      } else {
        this.tasks.dropCache(id);
      }
      return;
    }

    // Signatures agree: our own write (echo) or no change. Short-circuit here,
    // before touching the bytes.
    if (engineSig !== undefined && sig === engineSig) return;

    // The signature changed (or this is a task we have never cached): read +
    // parse the bytes.
    let disk: { sig: string; task: StoredTask } | null;
    try {
      disk = await this.tasks.readDisk(id);
    } catch (e) {
      // An unparseable task.json — a foreseeable human edit under hybrid
      // ownership. Never let one corrupt file brick open()/listTaskViews: if a
      // session is live, restore the engine's last-known-good record; otherwise
      // DROP it from the cache, leaving the bad bytes on disk for the operator.
      const broken = this.tasks.peek(id);
      const msg = e instanceof Error ? e.message : String(e);
      if (broken && this.sessions.hasLiveSession(id)) {
        this.logger.warn(
          `reconcile: ${id} has an unparseable task.json while a session is live; ` +
            `restoring engine state (${msg})`,
        );
        await this.tasks.writeTask(broken);
        this.clearCorruptMemo(id);
      } else {
        // Idle corrupt task.json: drop it from the cache so it VANISHES from
        // views — identical behaviour whether or not the task was previously
        // cached (C3). A never-cached corrupt file is already absent; this makes
        // a was-cached-then-corrupted file absent too, rather than rendering
        // stale last-known-good data. A clean re-parse re-admits the task.
        this.tasks.dropCache(id);
        if (this.corruptWarned.get(id) !== sig) {
          // De-dupe: warn ONCE per file signature, not once per reconcile, so a
          // polling client cannot spin the daemon log on a persistently-corrupt
          // idle task.json (M2). A later good (or differently-broken) write
          // clears or re-arms the memo via its new signature.
          this.logger.warn(`reconcile: dropping ${id} — unparseable task.json (${msg})`);
          this.corruptWarned.set(id, sig);
        }
      }
      return;
    }
    if (!disk) {
      // Raced an external deletion between the probe and the read; the next
      // reconcile/watcher event observes the now-absent file.
      this.clearCorruptMemo(id);
      return;
    }

    // The bytes parsed — this file is healthy, forget any prior corrupt warning.
    this.clearCorruptMemo(id);

    if (!engine) {
      // A task we have never seen: accept it as-is.
      this.tasks.setCache(id, disk.sig, disk.task);
      return;
    }

    if (this.sessions.hasLiveSession(id) && engineOwnedFieldsDiffer(engine, disk.task)) {
      this.logger.warn(
        `reconcile: rejected external edit to engine-owned fields (status/step/workflow) of ${id} ` +
          `(live session); restoring engine state`,
      );
      // Restore the engine-owned triple; keep externally-edited human fields
      // (title/description/blocked_by) so a bundled human edit is not lost
      // (plan §3.7(2): those fields are accepted, status/step/workflow are not).
      // When those human fields actually changed, bump `updated_at` so clients
      // diffing on it notice the accepted half; otherwise keep the engine's.
      const humanFieldsChanged =
        disk.task.title !== engine.title ||
        disk.task.description !== engine.description ||
        !sameIdList(disk.task.blocked_by, engine.blocked_by);
      const restored: StoredTask = {
        ...disk.task,
        status: engine.status,
        step: engine.step,
        workflow: engine.workflow,
        updated_at: humanFieldsChanged ? this.clock() : engine.updated_at,
      };
      await this.tasks.writeTask(restored);
      return;
    }

    // Accept the external edit.
    this.tasks.setCache(id, disk.sig, disk.task);
  }

  // -- task mutations ------------------------------------------------------

  /** Creates a task (`status:"new"`, unenrolled). */
  async createTask(input: {
    title: string;
    description?: string;
    blocked_by?: string[];
  }): Promise<TaskView> {
    const id = newTaskId();
    await this.locks.run(id, async () => {
      const now = this.clock();
      const task: StoredTask = {
        id,
        title: input.title,
        description: input.description ?? "",
        status: "new",
        workflow: null,
        step: null,
        blocked_by: input.blocked_by ? [...input.blocked_by] : [],
        created_at: now,
        updated_at: now,
      };
      await this.tasks.writeTask(task);
    });
    return this.taskView(id);
  }

  /** Updates the human-editable fields (`title` / `description`). */
  async updateTask(id: string, patch: { title?: string; description?: string }): Promise<TaskView> {
    await this.mutateTask(id, (t) => {
      if (patch.title !== undefined) t.title = patch.title;
      if (patch.description !== undefined) t.description = patch.description;
    });
    return this.taskView(id);
  }

  /**
   * Sets the engine-owned position (`status` / `step` / `workflow`). This is
   * the write the scheduler/engine make on enroll/transit (P4); P2 exposes it so
   * tasks can become enrolled (and thus eligible for the ownership policy).
   */
  async setPosition(
    id: string,
    pos: { status: TaskStatus; workflow: string | null; step: string | null },
  ): Promise<TaskView> {
    await this.mutateTask(id, (t) => {
      t.status = pos.status;
      t.workflow = pos.workflow;
      t.step = pos.step;
    });
    return this.taskView(id);
  }

  /**
   * Adds a dependency edge (`id` is blocked by `blockerId`). Idempotent.
   *
   * A self-block is rejected (it would leave the task permanently `blocked`).
   * An unknown `blockerId` is tolerated on disk — blockers may be created later,
   * so existence is NOT enforced here; the rendered view intentionally hides a
   * dangling edge (a `blocked_by` id with no matching task) until its blocker
   * exists (see {@link buildView}).
   */
  async block(id: string, blockerId: string): Promise<TaskView> {
    if (blockerId === id) throw new Error(`a task cannot block itself: ${id}`);
    await this.mutateTask(id, (t) => {
      if (!t.blocked_by.includes(blockerId)) t.blocked_by.push(blockerId);
    });
    return this.taskView(id);
  }

  /** Removes a dependency edge. Idempotent. */
  async unblock(id: string, blockerId: string): Promise<TaskView> {
    await this.mutateTask(id, (t) => {
      t.blocked_by = t.blocked_by.filter((b) => b !== blockerId);
    });
    return this.taskView(id);
  }

  /** Read-modify-write a task's stored record under its lock, bumping `updated_at`. */
  private async mutateTask(id: string, apply: (t: StoredTask) => void): Promise<void> {
    await this.locks.run(id, async () => {
      const current = await this.requireLocked(id);
      const next: StoredTask = { ...current, blocked_by: [...current.blocked_by] };
      apply(next);
      next.id = current.id;
      next.created_at = current.created_at;
      next.updated_at = this.clock();
      await this.tasks.writeTask(next);
    });
  }

  /**
   * Resolves the base record for a mutation. The caller already holds the
   * per-task lock, so we reconcile FIRST (under that same lock) and only then
   * peek the cache (C1). This is what keeps a mutation building on the CURRENT
   * disk state: reconcileLocked folds a pending accepted external edit
   * (title/description/blocked_by) into the cache, and rejects+restores an
   * illegal engine-owned external edit — so neither a stale cache resurrects an
   * overwritten field, nor does a `read()` launder an illegal edit into this
   * write. After reconcile a valid on-disk task is always cached, so a plain
   * peek suffices; a missing entry means the task was (validly) deleted.
   */
  private async requireLocked(id: string): Promise<StoredTask> {
    await this.reconcileLocked(id);
    const t = this.tasks.peek(id);
    if (!t) throw new Error(`task not found: ${id}`);
    return t;
  }

  /** Whether a `task.json` exists for `id` (cache-first, then disk). */
  private async taskExists(id: string): Promise<boolean> {
    if (this.tasks.peek(id)) return true;
    return (await this.tasks.read(id)) !== null;
  }

  // -- comment mutations ---------------------------------------------------

  /** Appends a comment to a task. Throws if the task does not exist. */
  async addComment(id: string, input: { author: string; text: string }): Promise<Comment> {
    return this.locks.run(`${id}::comments`, async () => {
      // Guard against orphaning a `comments.jsonl` under a dir with no task.json
      // (which `listIdsOnDisk` then skips, hiding the file from every read).
      if (!(await this.taskExists(id))) throw new Error(`task not found: ${id}`);
      const comments = await this.tasks.readComments(id);
      const now = this.clock();
      const comment: Comment = {
        // Collision-check against the ids already on this task: the comment id is
        // the edit/delete key, so a duplicate within the file would retarget the
        // wrong comment (M1).
        id: newCommentId(new Set(comments.map((c) => c.id))),
        author: input.author,
        text: input.text,
        created_at: now,
        updated_at: now,
      };
      comments.push(comment);
      await this.tasks.writeComments(id, comments);
      return comment;
    });
  }

  /** Edits a comment's text (atomic full-file rewrite). Throws if the task is gone. */
  async editComment(id: string, commentId: string, text: string): Promise<Comment | null> {
    return this.locks.run(`${id}::comments`, async () => {
      if (!(await this.taskExists(id))) throw new Error(`task not found: ${id}`);
      const comments = await this.tasks.readComments(id);
      const found = comments.find((c) => c.id === commentId);
      if (!found) return null;
      found.text = text;
      found.updated_at = this.clock();
      await this.tasks.writeComments(id, comments);
      return found;
    });
  }

  /** Deletes a comment (atomic full-file rewrite). Returns whether it existed. */
  async deleteComment(id: string, commentId: string): Promise<boolean> {
    return this.locks.run(`${id}::comments`, async () => {
      if (!(await this.taskExists(id))) throw new Error(`task not found: ${id}`);
      const comments = await this.tasks.readComments(id);
      const next = comments.filter((c) => c.id !== commentId);
      if (next.length === comments.length) return false;
      await this.tasks.writeComments(id, next);
      return true;
    });
  }

  /** Lists a task's comments (in file order). */
  async listComments(id: string): Promise<Comment[]> {
    return this.tasks.readComments(id);
  }

  // -- reads / derived views -----------------------------------------------

  /** The enriched view of one task (reconciles it first). Throws if absent. */
  async taskView(id: string): Promise<TaskView> {
    await this.reconcileTask(id);
    // After reconcile a valid on-disk task is always cached; peek-only keeps a
    // corrupt/unparseable file from throwing a raw parse error here (R1).
    const task = this.tasks.peek(id);
    if (!task) throw new Error(`task not found: ${id}`);
    const all = await this.snapshotAll();
    const reverse = buildReverseIndex(all);
    return this.buildView(task, all, reverse, await this.commentCount(id));
  }

  /** Enriched views of all tasks (reconciles the whole project first). */
  async listTaskViews(filter?: TaskFilter): Promise<TaskView[]> {
    await this.reconcileAll();
    const all = await this.snapshotAll();
    // Build the reverse `blocked_by` index ONCE (O(N+E)) instead of rescanning
    // every task for each task's `blocks` (O(N²)) — the hot list path stays linear.
    const reverse = buildReverseIndex(all);
    const views: TaskView[] = [];
    for (const task of all.values()) {
      const count = await this.commentCount(task.id);
      views.push(this.buildView(task, all, reverse, count));
    }
    views.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    return filter ? views.filter((v) => matchesFilter(v, filter)) : views;
  }

  /**
   * Lightweight scheduling rows for every `work` task (the scheduler hot path).
   *
   * Reconciles the whole project first — so external edits (a task unblocked or
   * enrolled directly on disk) are picked up, which the engine's periodic safety
   * rescan relies on — then returns only {@link SchedulingRow} fields. Unlike
   * {@link listTaskViews} it reads NO `comments.jsonl` and builds NO reverse
   * `blocks` index (the O(N) costs the scheduler does not need).
   */
  async listSchedulable(): Promise<SchedulingRow[]> {
    await this.reconcileAll();
    const rows: SchedulingRow[] = [];
    for (const id of this.tasks.cachedIds()) {
      const t = this.tasks.peek(id);
      if (!t || t.status !== "work") continue;
      rows.push(this.schedulingRowFor(t));
    }
    rows.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    return rows;
  }

  /**
   * A fresh {@link SchedulingRow} for one task (reconciles just that task). The
   * scheduler re-reads this immediately before claiming a task so it never
   * dispatches at a step that advanced under an `await` since the enumerating
   * scan snapshot was taken. Returns `null` if the task is gone.
   */
  async schedulingRow(id: string): Promise<SchedulingRow | null> {
    await this.reconcileTask(id);
    const t = this.tasks.peek(id);
    return t ? this.schedulingRowFor(t) : null;
  }

  /**
   * Derives a {@link SchedulingRow} from a stored record. `blocked` is computed
   * from the blockers' CACHED statuses (a dangling/unknown blocker is treated as
   * not-open, mirroring {@link buildView}); a scan only ever needs an answer good
   * enough to gate this pass — the next reconciled scan corrects any staleness.
   */
  private schedulingRowFor(t: StoredTask): SchedulingRow {
    const blocked = t.blocked_by.some((bid) => {
      const b = this.tasks.peek(bid);
      return b !== undefined && OPEN_STATUSES.has(b.status);
    });
    return { id: t.id, status: t.status, workflow: t.workflow, step: t.step, blocked };
  }

  /** A reconciled map of every cached task (id → record). */
  private async snapshotAll(): Promise<Map<string, StoredTask>> {
    const map = new Map<string, StoredTask>();
    for (const id of this.tasks.cachedIds()) {
      const t = this.tasks.peek(id);
      if (t) map.set(id, t);
    }
    return map;
  }

  /**
   * Comment count for a task, warning ONCE per `comments.jsonl` signature when
   * the lenient parser had to skip a malformed line (C5) — a silently-dropped
   * human comment is otherwise invisible (`comments.jsonl` is a high-churn human
   * channel, so a warn-per-read would spam; de-duping per signature, like the
   * corrupt task.json memo M2, gives one signal per bad edit). The skip is
   * already isolated by `parseCommentsLenient` (R2); this only surfaces it.
   */
  private async commentCount(id: string): Promise<number> {
    const { count, skipped, sig } = await this.tasks.commentStats(id);
    if (sig === null || skipped === 0) {
      this.commentSkipWarned.delete(id);
      return count;
    }
    if (this.commentSkipWarned.get(id) !== sig) {
      this.logger.warn(
        `comments: skipped ${skipped} malformed line(s) in ${id}/comments.jsonl`,
      );
      this.commentSkipWarned.set(id, sig);
    }
    return count;
  }

  /**
   * Builds the derived {@link TaskView} from a stored record + project snapshot.
   *
   * `reverse` is the precomputed blocker→blocked index. A `blocked_by` id with
   * no matching task in `all` (a dangling/unknown blocker) is intentionally
   * filtered out of the rendered `blocked_by`/`blocks` until its blocker exists.
   */
  private buildView(
    task: StoredTask,
    all: Map<string, StoredTask>,
    reverse: Map<string, TaskRef[]>,
    commentCount: number,
  ): TaskView {
    const blockedBy = task.blocked_by
      .map((bid) => all.get(bid))
      .filter((t): t is StoredTask => t !== undefined)
      .map((t) => ({ id: t.id, status: t.status }));
    const blocked = blockedBy.some((ref) => OPEN_STATUSES.has(ref.status));
    const blocks = reverse.get(task.id) ?? [];
    return {
      id: task.id,
      title: task.title,
      description: task.description,
      status: task.status,
      workflow: task.workflow,
      step: task.step,
      blocked,
      blocked_by: blockedBy,
      blocks,
      comment_count: commentCount,
      created_at: task.created_at,
      updated_at: task.updated_at,
    };
  }
}

/** Do the engine-owned fields differ between two records? */
function engineOwnedFieldsDiffer(a: StoredTask, b: StoredTask): boolean {
  return a.status !== b.status || a.step !== b.step || a.workflow !== b.workflow;
}

/** Order-sensitive equality of two id lists (cheap `blocked_by` diff). */
function sameIdList(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

/**
 * Reverse `blocked_by` index: blockerId → the tasks it blocks, each list sorted
 * by id. Built once per list/view call (O(N+E)) so `buildView` only looks up its
 * own id. Every task id keys a distinct array, so views never alias each other.
 */
function buildReverseIndex(all: Map<string, StoredTask>): Map<string, TaskRef[]> {
  const reverse = new Map<string, TaskRef[]>();
  for (const t of all.values()) {
    for (const bid of t.blocked_by) {
      let arr = reverse.get(bid);
      if (!arr) {
        arr = [];
        reverse.set(bid, arr);
      }
      arr.push({ id: t.id, status: t.status });
    }
  }
  for (const arr of reverse.values()) {
    arr.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  }
  return reverse;
}

/** Applies a `TaskFilter` to a rendered view. */
function matchesFilter(v: TaskView, f: TaskFilter): boolean {
  if (f.status !== undefined) {
    const wanted = Array.isArray(f.status) ? f.status : [f.status];
    if (!wanted.includes(v.status)) return false;
  }
  if (f.workflow !== undefined && v.workflow !== f.workflow) return false;
  if (f.step !== undefined && v.step !== f.step) return false;
  if (f.blocked !== undefined && v.blocked !== f.blocked) return false;
  return true;
}
