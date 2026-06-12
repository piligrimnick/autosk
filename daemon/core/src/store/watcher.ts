/**
 * The `.autosk/tasks` fs watcher (plan §3.7(2)).
 *
 * Hybrid ownership needs the daemon to notice *external* edits to task files.
 * `fs.watch` (recursive) gives low-latency notification; a slow periodic
 * rescan is the safety net for any event the platform drops (and the only
 * mechanism on runtimes without recursive watch). Both funnel into the same
 * `Store` reconciliation, so echo suppression + ownership policy live in one
 * place — the watcher just says "task X may have changed" / "rescan".
 *
 * The watcher is intentionally dumb: it does no diffing. Reconciliation
 * (signature compare → accept/reject) is the Store's job.
 */

import { watch, type FSWatcher } from "node:fs";

export interface WatcherCallbacks {
  /** A specific task's files may have changed. */
  onTaskTouched(taskId: string): void;
  /** The periodic safety net fired: reconcile everything. */
  onRescan(): void;
}

export interface WatcherOptions {
  /** Safety-rescan interval in ms. `0` disables it (tests with a live watcher). */
  rescanIntervalMs?: number;
  /** Coalescing delay before a touched task is reported, in ms. */
  debounceMs?: number;
}

export class TasksWatcher {
  private readonly tasksDir: string;
  private readonly cbs: WatcherCallbacks;
  private readonly rescanIntervalMs: number;
  private readonly debounceMs: number;

  private watcher: FSWatcher | null = null;
  private rescanTimer: ReturnType<typeof setInterval> | null = null;
  private pending = new Map<string, ReturnType<typeof setTimeout>>();
  private closed = false;

  constructor(tasksDir: string, cbs: WatcherCallbacks, opts: WatcherOptions = {}) {
    this.tasksDir = tasksDir;
    this.cbs = cbs;
    this.rescanIntervalMs = opts.rescanIntervalMs ?? 5_000;
    this.debounceMs = opts.debounceMs ?? 20;
  }

  /** Starts watching. Best-effort: a watch failure falls back to rescans only. */
  start(): void {
    this.closed = false;
    try {
      this.watcher = watch(this.tasksDir, { recursive: true }, (_event, filename) => {
        if (this.closed || !filename) return;
        const taskId = firstSegment(filename.toString());
        if (taskId) this.touch(taskId);
      });
      this.watcher.on("error", () => {
        // A watch error must never crash the daemon; rescans cover us.
      });
    } catch {
      this.watcher = null;
    }
    if (this.rescanIntervalMs > 0) {
      this.rescanTimer = setInterval(() => {
        if (!this.closed) this.cbs.onRescan();
      }, this.rescanIntervalMs);
      this.rescanTimer.unref?.();
    }
  }

  private touch(taskId: string): void {
    const existing = this.pending.get(taskId);
    if (existing) clearTimeout(existing);
    const timer = setTimeout(() => {
      this.pending.delete(taskId);
      if (!this.closed) this.cbs.onTaskTouched(taskId);
    }, this.debounceMs);
    timer.unref?.();
    this.pending.set(taskId, timer);
  }

  /** Stops watching and clears all timers. Idempotent. */
  close(): void {
    this.closed = true;
    if (this.watcher) {
      this.watcher.close();
      this.watcher = null;
    }
    if (this.rescanTimer) {
      clearInterval(this.rescanTimer);
      this.rescanTimer = null;
    }
    for (const timer of this.pending.values()) clearTimeout(timer);
    this.pending.clear();
  }
}

/** The first path segment of a (possibly nested) watch filename. */
function firstSegment(filename: string): string | null {
  const normalized = filename.split(/[\\/]/).filter((s) => s.length > 0);
  const seg = normalized[0];
  if (!seg || seg.startsWith(".")) return null;
  return seg;
}
