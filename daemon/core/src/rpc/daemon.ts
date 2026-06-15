/**
 * The RPC daemon orchestrator (plan §4 / §3.7(5)).
 *
 * Owns the {@link Engine} + {@link ProjectManager}, the live-connection registry,
 * the method dispatch table, the notification fan-out, and the idle-shutdown
 * watchdog. It is transport-agnostic: {@link RpcServer} feeds it
 * {@link Connection}s and calls {@link dispatch}; tests drive the same surface.
 *
 * Notification sourcing (plan §4):
 *  - `session-event` ← {@link Engine} events (status/done/error/message).
 *  - `task-changed`  ← the {@link Store.onTaskChange} seam (covers engine writes,
 *    direct-RPC writes, AND external file edits the watcher reconciles). The
 *    engine's own `EngineEvent` `task-changed` is deliberately IGNORED here to
 *    avoid double-emitting — every task mutation funnels through the store.
 *  - `project-changed` ← {@link ProjectManager} mutations in the RPC handlers
 *    (add / remove / init), which is not an engine event.
 */

import { basename } from "node:path";

import {
  ErrorCodes,
  RPC_METHODS,
  type ProjectChangedParams,
  type ProjectInfo,
  type ResultOf,
  type RpcMethod,
  type SessionChangedParams,
  type SessionMeta,
  type StepTarget,
  type TaskChangedParams,
  type TaskFilter,
  type TaskView,
} from "@autosk/sdk";

import { Engine, type EngineEvent } from "../engine/index.ts";
import { canonicalize, resolveProjectRoot, type ProjectHandle, type ProjectManager } from "../project/index.ts";
import { consoleLogger, type Logger } from "../store/index.ts";
import type { Store } from "../store/store.ts";
import { Connection, SessionSubscription } from "./connection.ts";
import { RpcError } from "./errors.ts";
import { commit, VERSION } from "../version.ts";

/**
 * The method dispatch table, typed as a mapped type over {@link RpcMethod}: the
 * KEY set is pinned to `RpcMethod` (which drives the conformance no-drift test)
 * AND each handler's RETURN is bound to that method's `ResultOf<M>`, so a
 * handler that produces the wrong wire shape fails typecheck at the source
 * rather than slipping past into a behaviour test (review #2). `params` stays
 * `unknown` on purpose — untrusted wire input is validated at runtime via the
 * `asObj`/`reqString`/… guards; only the trusted result is checked here.
 */
type HandlerTable = { [M in RpcMethod]: (params: unknown, conn: Connection) => Promise<ResultOf<M>> };

/** The type-erased handler the string-keyed {@link Daemon.dispatch} path sees. */
type Handler = (params: unknown, conn: Connection) => Promise<unknown>;

export interface DaemonOptions {
  projectManager: ProjectManager;
  engine: Engine;
  /** Expected TCP auth token (`null` ⇒ TCP auth always fails; UDS exempt). */
  token?: string | null;
  logger?: Logger;
  /** Idle-shutdown window in ms (`null`/`0` disables). */
  idleWindowMs?: number | null;
  /** Idle-check cadence in ms (default 5000). */
  idleCheckMs?: number;
  /** Delay before the shutdown hook runs, so a `meta.shutdown` reply flushes first (default 15ms). */
  shutdownDelayMs?: number;
}

export class Daemon {
  readonly engine: Engine;
  readonly projectManager: ProjectManager;

  private readonly token: string | null;
  private readonly logger: Logger;
  private readonly idleWindowMs: number | null;
  private readonly idleCheckMs: number;
  private readonly shutdownDelayMs: number;

  private readonly connections = new Set<Connection>();
  private readonly wiredRoots = new Set<string>();
  private readonly handlers: HandlerTable;
  private readonly engineUnsub: () => void;

  private shutdownHook: (() => void) | null = null;
  private shuttingDown = false;
  private idleTimer: ReturnType<typeof setInterval> | null = null;
  private idleSince: number | null = null;

  constructor(opts: DaemonOptions) {
    this.engine = opts.engine;
    this.projectManager = opts.projectManager;
    this.token = opts.token ?? null;
    this.logger = opts.logger ?? consoleLogger;
    this.idleWindowMs = opts.idleWindowMs ?? null;
    this.idleCheckMs = opts.idleCheckMs ?? 5_000;
    this.shutdownDelayMs = opts.shutdownDelayMs ?? 15;
    this.handlers = this.buildHandlers();
    this.engineUnsub = this.engine.on((ev) => this.onEngineEvent(ev));
  }

  // -- connection registry -------------------------------------------------

  addConnection(conn: Connection): void {
    this.connections.add(conn);
  }

  removeConnection(conn: Connection): void {
    this.connections.delete(conn);
    conn.sessionSubs.clear();
  }

  /** Live client connections (UDS + TCP) — the idle-shutdown predicate. */
  get connectionCount(): number {
    return this.connections.size;
  }

  // -- dispatch ------------------------------------------------------------

  async dispatch(method: string, params: unknown, conn: Connection): Promise<unknown> {
    const handler = (this.handlers as Record<string, Handler | undefined>)[method];
    if (!handler) {
      throw new RpcError(ErrorCodes.METHOD_NOT_FOUND, `unknown method: ${method}`);
    }
    return handler(params, conn);
  }

  /** The registered method names — the conformance test diffs this vs `RPC_METHODS`. */
  registeredMethods(): RpcMethod[] {
    return Object.keys(this.handlers) as RpcMethod[];
  }

  // -- lifecycle -----------------------------------------------------------

  /** Starts the idle-shutdown watchdog (no-op when disabled). */
  start(): void {
    this.startIdleWatchdog();
  }

  /** Registers the teardown the daemon invokes on `meta.shutdown` / idle-timeout. */
  onShutdownRequested(hook: () => void): void {
    this.shutdownHook = hook;
  }

  /** Schedules the shutdown hook once (after a short delay so a reply can flush). */
  requestShutdown(): void {
    if (this.shuttingDown) return;
    this.shuttingDown = true;
    this.stopIdleWatchdog();
    const hook = this.shutdownHook;
    const timer = setTimeout(() => {
      try {
        hook?.();
      } catch (e) {
        this.logger.error(`shutdown hook threw: ${errStr(e)}`);
      }
    }, this.shutdownDelayMs);
    timer.unref?.();
  }

  /** Tears down the engine + project stores (called by the shutdown hook). */
  async close(): Promise<void> {
    this.stopIdleWatchdog();
    this.engineUnsub();
    this.engine.stop();
    await this.projectManager.close();
  }

  // -- notification fan-out ------------------------------------------------

  private onEngineEvent(ev: EngineEvent): void {
    // task-changed is sourced from the store seam (see class doc) — ignore the
    // engine's, which would only duplicate it.
    if (ev.type !== "session-event") return;
    // 1. Per-session transcript subscribers (session.subscribe): replay-then-tail.
    for (const conn of this.connections) {
      const sub = conn.sessionSubs.get(ev.session_id);
      if (sub && sub.root === ev.root) {
        sub.onEvent({ kind: ev.kind, meta: ev.meta, error: ev.error });
      }
    }
    // 2. Project-scoped session lifecycle channel (session.subscribeProject):
    //    every non-`message` frame carries the decorated meta, so a subscriber
    //    sees a session appear (queued), start (running), and finish (terminal)
    //    live — WITHOUT having to know a session id to subscribe per-session.
    //    `message` frames are the transcript tail and belong only to §1.
    if (ev.kind !== "message" && ev.meta) {
      this.emitSessionChanged(ev.root, ev.meta);
    }
  }

  private emitSessionChanged(root: string, session: SessionMeta): void {
    const params: SessionChangedParams = { root, session };
    for (const conn of this.connections) {
      if (conn.sessionRoots.has(root)) conn.send({ method: "session-changed", params });
    }
  }

  private onStoreTaskChange(root: string, store: Store, id: string): void {
    void this.emitTaskChanged(root, store, id);
  }

  private async emitTaskChanged(root: string, store: Store, id: string): Promise<void> {
    let task: TaskView;
    try {
      task = await store.taskView(id);
    } catch {
      // The task vanished (external delete): there is no view to push, and the
      // proto `task-changed` always carries a task — clients re-list to notice.
      return;
    }
    const params: TaskChangedParams = { root, task };
    for (const conn of this.connections) {
      if (conn.taskRoots.has(root)) conn.send({ method: "task-changed", params });
    }
  }

  private emitProjectChanged(project: ProjectInfo): void {
    const params: ProjectChangedParams = { project };
    for (const conn of this.connections) {
      if (conn.wantsProject) conn.send({ method: "project-changed", params });
    }
  }

  // -- project resolution --------------------------------------------------

  /**
   * Resolves a `{cwd}` to an opened project, ensures the engine schedules over
   * it, and wires the store's task-change seam to the notification fan-out
   * (once per root). Opening also starts the project's fs watcher, so a
   * `task.subscribe`/`project.diagnostics`/read on a project is what makes its
   * external-edit `task-changed` notifications flow.
   */
  private async resolveHandle(cwd: string): Promise<ProjectHandle> {
    const handle = await this.projectManager.resolve(cwd);
    if (!this.wiredRoots.has(handle.root)) {
      this.wiredRoots.add(handle.root);
      handle.store.onTaskChange((id) => this.onStoreTaskChange(handle.root, handle.store, id));
    }
    await this.engine.addProject({ root: handle.root, store: handle.store, registry: handle.extensions });
    return handle;
  }

  // -- idle-shutdown -------------------------------------------------------

  private startIdleWatchdog(): void {
    if (!this.idleWindowMs || this.idleWindowMs <= 0 || this.idleTimer) return;
    this.idleSince = null;
    this.idleTimer = setInterval(() => void this.checkIdle(), this.idleCheckMs);
    this.idleTimer.unref?.();
  }

  private stopIdleWatchdog(): void {
    if (this.idleTimer) {
      clearInterval(this.idleTimer);
      this.idleTimer = null;
    }
  }

  private async checkIdle(): Promise<void> {
    if (this.shuttingDown || !this.idleWindowMs) return;
    let busy: boolean;
    try {
      busy = this.connections.size > 0 || (await this.hasPendingWork());
    } catch (e) {
      this.logger.warn(`idle check failed: ${errStr(e)}`);
      return;
    }
    if (busy) {
      this.idleSince = null;
      return;
    }
    const now = Date.now();
    if (this.idleSince === null) {
      this.idleSince = now;
      return;
    }
    if (now - this.idleSince >= this.idleWindowMs) {
      this.logger.info(`autoskd: idle for ${this.idleWindowMs}ms; shutting down`);
      this.requestShutdown();
    }
  }

  /**
   * The idle-shutdown "busy" predicate (plan §4, the v1 three conditions): any
   * queued/running session in the pool OR persisted across loaded projects, or
   * any `status=work` task across loaded projects.
   */
  private async hasPendingWork(): Promise<boolean> {
    const stats = this.engine.stats();
    if (stats.queued > 0 || stats.running > 0) return true;
    for (const handle of this.projectManager.loaded()) {
      for (const meta of handle.store.sessions.allMetas()) {
        if (meta.status === "queued" || meta.status === "running") return true;
      }
      const work = await handle.store.listSchedulable();
      if (work.length > 0) return true;
    }
    return false;
  }

  // -- handler table -------------------------------------------------------

  private buildHandlers(): HandlerTable {
    return {
      // ---- meta ----------------------------------------------------------
      "meta.version": async () => ({ version: VERSION, commit: commit() }),
      "meta.auth": async (params, conn) => {
        if (!conn.isTcp) return { ok: true }; // UDS is exempt
        const token = asObj(params).token;
        if (this.token && typeof token === "string" && token.length > 0 && token === this.token) {
          conn.authed = true;
          return { ok: true };
        }
        throw new RpcError(ErrorCodes.INVALID_REQUEST, "invalid or missing token");
      },
      "meta.healthz": async () => {
        const stats = this.engine.stats();
        const projects = this.projectManager.loaded().map((handle) => {
          let queued = 0;
          let running = 0;
          for (const meta of handle.store.sessions.allMetas()) {
            if (meta.status === "queued") queued++;
            else if (meta.status === "running") running++;
          }
          return { root: handle.root, queued, running, opened_at: handle.opened_at };
        });
        return { ok: true, workers: stats.workers, queued: stats.queued, running: stats.running, projects };
      },
      "meta.shutdown": async () => {
        this.requestShutdown();
        return { ok: true };
      },

      // ---- project -------------------------------------------------------
      "project.list": async () => this.projectManager.listProjects(),
      "project.add": async (params) => {
        const o = asObj(params);
        const info = await this.projectManager.addProject(reqCwd(o), optString(o, "name"));
        this.emitProjectChanged(info);
        return info;
      },
      "project.remove": async (params) => {
        const cwd = reqCwd(asObj(params));
        let root: string;
        try {
          root = await resolveProjectRoot(cwd);
        } catch {
          root = await canonicalize(cwd).catch(() => cwd);
        }
        const removed = await this.projectManager.removeProject(cwd);
        if (removed) this.emitProjectChanged({ root, name: basename(root) });
        return { ok: removed };
      },
      "project.init": async (params) => {
        const cwd = reqCwd(asObj(params));
        const info = await this.projectManager.initProject(cwd);
        // Register so the project surfaces in `project.list` (matches v1 init).
        await this.projectManager.addProject(info.root).catch(() => undefined);
        this.emitProjectChanged(info);
        return info;
      },
      "project.diagnostics": async (params) => {
        const handle = await this.resolveHandle(reqCwd(asObj(params)));
        return { root: handle.root, extensions: handle.extensions.diagnostics };
      },
      "project.subscribe": async (_params, conn) => {
        // Registry watching is global (project-changed is not project-scoped), so
        // this does NOT require an open project — a GUI can watch before one exists.
        conn.wantsProject = true;
        return { ok: true };
      },
      "project.unsubscribe": async (_params, conn) => {
        conn.wantsProject = false;
        return { ok: true };
      },

      // ---- task ----------------------------------------------------------
      "task.list": async (params) => {
        const o = asObj(params);
        const handle = await this.resolveHandle(reqCwd(o));
        return handle.store.listTaskViews(parseTaskFilter(o.filter));
      },
      "task.get": async (params) => {
        const o = asObj(params);
        const handle = await this.resolveHandle(reqCwd(o));
        return this.getTaskView(handle.store, reqString(o, "id"));
      },
      "task.create": async (params) => {
        const o = asObj(params);
        const handle = await this.resolveHandle(reqCwd(o));
        return handle.store.createTask({
          title: reqString(o, "title"),
          description: optString(o, "description"),
          blocked_by: parseIdArray(o.blocked_by, "blocked_by"),
        });
      },
      "task.update": async (params) => {
        const o = asObj(params);
        const id = reqString(o, "id");
        const patch: { title?: string; description?: string } = {};
        if (o.title !== undefined) patch.title = asString(o.title, "title");
        if (o.description !== undefined) patch.description = asString(o.description, "description");
        const handle = await this.resolveHandle(reqCwd(o));
        return mapTaskNotFound(() => handle.store.updateTask(id, patch));
      },
      "task.enroll": async (params) => {
        const o = asObj(params);
        const id = reqString(o, "id");
        const workflow = reqString(o, "workflow");
        const step = optString(o, "step");
        const handle = await this.resolveHandle(reqCwd(o));
        return this.engine.enroll(handle.root, id, step !== undefined ? { workflow, step } : { workflow });
      },
      "task.resume": async (params) => {
        const o = asObj(params);
        const id = reqString(o, "id");
        const to = parseStepTarget(o.to);
        const handle = await this.resolveHandle(reqCwd(o));
        return this.engine.resume(handle.root, id, to);
      },
      "task.done": async (params) => this.taskTerminal(params, "done"),
      "task.cancel": async (params) => this.taskTerminal(params, "cancel"),
      "task.reopen": async (params) => {
        const o = asObj(params);
        const id = reqString(o, "id");
        const handle = await this.resolveHandle(reqCwd(o));
        const view = await this.getTaskView(handle.store, id);
        if (view.status !== "done" && view.status !== "cancel") {
          throw new RpcError(ErrorCodes.CONFLICT, `cannot reopen ${id}: status is ${view.status} (expected done/cancel)`);
        }
        // Enrolled ⇒ park to `human` (resumable via task.resume); never enrolled
        // ⇒ back to the `new` backlog (re-enroll to continue).
        if (view.workflow !== null && view.step !== null) {
          return handle.store.setPosition(id, { status: "human", workflow: view.workflow, step: view.step });
        }
        return handle.store.setPosition(id, { status: "new", workflow: null, step: null });
      },
      "task.block": async (params) => {
        const o = asObj(params);
        const id = reqString(o, "id");
        const blocker = reqString(o, "blocked_by");
        const handle = await this.resolveHandle(reqCwd(o));
        return mapTaskNotFound(() => handle.store.block(id, blocker));
      },
      "task.unblock": async (params) => {
        const o = asObj(params);
        const id = reqString(o, "id");
        const blocker = reqString(o, "blocked_by");
        const handle = await this.resolveHandle(reqCwd(o));
        return mapTaskNotFound(() => handle.store.unblock(id, blocker));
      },
      "task.comment.add": async (params) => {
        const o = asObj(params);
        const taskId = reqString(o, "task_id");
        const text = reqString(o, "text");
        const author = optString(o, "author") ?? "human";
        const handle = await this.resolveHandle(reqCwd(o));
        return mapTaskNotFound(() => handle.store.addComment(taskId, { author, text }));
      },
      "task.comment.list": async (params) => {
        const o = asObj(params);
        const handle = await this.resolveHandle(reqCwd(o));
        return handle.store.listComments(reqString(o, "task_id"));
      },
      "task.comment.edit": async (params) => {
        const o = asObj(params);
        const taskId = reqString(o, "task_id");
        const commentId = reqString(o, "comment_id");
        const text = reqString(o, "text");
        const handle = await this.resolveHandle(reqCwd(o));
        const edited = await mapTaskNotFound(() => handle.store.editComment(taskId, commentId, text));
        if (!edited) throw new RpcError(ErrorCodes.NOT_FOUND, `comment not found: ${commentId}`);
        return edited;
      },
      "task.comment.delete": async (params) => {
        const o = asObj(params);
        const taskId = reqString(o, "task_id");
        const commentId = reqString(o, "comment_id");
        const handle = await this.resolveHandle(reqCwd(o));
        const removed = await mapTaskNotFound(() => handle.store.deleteComment(taskId, commentId));
        return { ok: removed };
      },
      "task.subscribe": async (params, conn) => {
        // Resolving OPENS the project (its watcher runs), which is what makes
        // external-edit `task-changed` notifications flow for this root.
        const handle = await this.resolveHandle(reqCwd(asObj(params)));
        conn.taskRoots.add(handle.root);
        return { ok: true };
      },
      "task.unsubscribe": async (params, conn) => {
        const cwd = reqCwd(asObj(params));
        try {
          conn.taskRoots.delete(await resolveProjectRoot(cwd));
        } catch {
          // unknown/removed project — nothing to unsubscribe
        }
        return { ok: true };
      },

      // ---- registry ------------------------------------------------------
      "registry.workflow.list": async (params) => {
        const handle = await this.resolveHandle(reqCwd(asObj(params)));
        return handle.extensions.listWorkflows();
      },
      "registry.workflow.get": async (params) => {
        const o = asObj(params);
        const handle = await this.resolveHandle(reqCwd(o));
        const info = handle.extensions.getWorkflowInfo(reqString(o, "name"));
        if (!info) throw new RpcError(ErrorCodes.NOT_FOUND, `workflow not found: ${reqString(o, "name")}`);
        return info;
      },

      // ---- extension management (autosk ext) -----------------------------
      // A GLOBAL install/update does NOT require an open project (only `cwd`,
      // used to resolve a relative local path); `-l/--local` (`scope:"project"`)
      // requires a project at cwd.
      "extension.install": async (params) => {
        const o = asObj(params);
        return this.projectManager.installExtension(reqCwd(o), reqString(o, "source"), optBool(o, "local") ?? false);
      },
      "extension.remove": async (params) => {
        const o = asObj(params);
        return this.projectManager.removeExtension(reqCwd(o), reqString(o, "source"), optBool(o, "local") ?? false);
      },
      "extension.list": async (params) => {
        const o = asObj(params);
        return this.projectManager.listExtensions(reqCwd(o));
      },
      "extension.update": async (params) => {
        const o = asObj(params);
        const scope = optString(o, "scope");
        if (scope !== undefined && scope !== "global" && scope !== "project") {
          throw new RpcError(ErrorCodes.INVALID_PARAMS, `invalid scope ${JSON.stringify(scope)}: use "global" or "project"`);
        }
        return this.projectManager.updateExtensions(reqCwd(o), {
          source: optString(o, "source"),
          scope,
          dryRun: optBool(o, "dry_run") ?? false,
        });
      },

      // ---- session -------------------------------------------------------
      "session.list": async (params) => {
        const o = asObj(params);
        const handle = await this.resolveHandle(reqCwd(o));
        const taskId = optString(o, "task_id");
        if (taskId) return handle.store.sessions.sessionsForTask(taskId);
        // Newest-first by id (UUIDv7 ids sort by creation), the default order
        // clients render top-to-bottom — matches sessionsForTask.
        return handle.store.sessions
          .allMetas()
          .sort((a, b) => (a.id < b.id ? 1 : a.id > b.id ? -1 : 0));
      },
      "session.get": async (params) => {
        const o = asObj(params);
        const handle = await this.resolveHandle(reqCwd(o));
        const meta = await handle.store.sessions.getMeta(reqString(o, "id"));
        if (!meta) throw new RpcError(ErrorCodes.NOT_FOUND, `session not found: ${reqString(o, "id")}`);
        return meta;
      },
      "session.transcript": async (params) => {
        const o = asObj(params);
        const id = reqString(o, "id");
        const handle = await this.resolveHandle(reqCwd(o));
        const meta = await handle.store.sessions.getMeta(id);
        if (!meta) throw new RpcError(ErrorCodes.NOT_FOUND, `session not found: ${id}`);
        const { lines, nextLine } = await handle.store.sessions.readTranscript(id, {
          fromLine: optInt(o, "from_line"),
          limit: optInt(o, "limit"),
        });
        return { entries: lines, next_line: nextLine };
      },
      "session.subscribe": async (params, conn) => {
        const o = asObj(params);
        const id = reqString(o, "id");
        const handle = await this.resolveHandle(reqCwd(o));
        const meta = await handle.store.sessions.getMeta(id);
        if (!meta) throw new RpcError(ErrorCodes.NOT_FOUND, `session not found: ${id}`);
        const sub = new SessionSubscription(handle.root, id, handle.store.sessions, conn, optInt(o, "from_line"));
        conn.sessionSubs.set(id, sub);
        sub.start();
        return { ok: true };
      },
      "session.unsubscribe": async (params, conn) => {
        conn.sessionSubs.delete(reqString(asObj(params), "id"));
        return { ok: true };
      },
      "session.subscribeProject": async (params, conn) => {
        // Resolving OPENS the project (engine scheduling + its fs watcher),
        // which is what makes its session lifecycle pushes flow for this root.
        // Project-scoped analogue of `task.subscribe`; the per-session
        // `session.subscribe` transcript tail is unchanged and independent.
        const handle = await this.resolveHandle(reqCwd(asObj(params)));
        conn.sessionRoots.add(handle.root);
        return { ok: true };
      },
      "session.unsubscribeProject": async (params, conn) => {
        const cwd = reqCwd(asObj(params));
        try {
          conn.sessionRoots.delete(await resolveProjectRoot(cwd));
        } catch {
          // unknown/removed project — nothing to unsubscribe
        }
        return { ok: true };
      },
      "session.input": async (params) => {
        const o = asObj(params);
        const id = reqString(o, "id");
        const message = reqString(o, "message");
        const kind = o.kind;
        if (kind !== "steer" && kind !== "followup") {
          throw new RpcError(ErrorCodes.INVALID_PARAMS, "kind must be 'steer' or 'followup'");
        }
        const handle = await this.resolveHandle(reqCwd(o));
        const { handled } = await this.engine.sessionInput(handle.root, id, { message, kind });
        // An absent steer/followup hook (or a still-queued / already-settled
        // session) cannot deliver the message → unsupported_by_agent (plan §3.4).
        if (!handled) throw new RpcError(ErrorCodes.CONFLICT, "unsupported_by_agent");
        return { ok: true };
      },
      "session.abort": async (params) => {
        const o = asObj(params);
        const handle = await this.resolveHandle(reqCwd(o));
        // abort ALWAYS acts on a live session; `handled:false` means only
        // "already settled, nothing to abort" — never unsupported_by_agent (plan §3.4).
        const { handled } = await this.engine.sessionAbort(handle.root, reqString(o, "id"));
        return { ok: handled };
      },
    };
  }

  // -- shared handler helpers ----------------------------------------------

  private async getTaskView(store: Store, id: string): Promise<TaskView> {
    try {
      return await store.taskView(id);
    } catch {
      throw new RpcError(ErrorCodes.NOT_FOUND, `task not found: ${id}`);
    }
  }

  /**
   * `task.done` / `task.cancel`: set the terminal status, keeping workflow/step.
   * Rejected with CONFLICT while a session is live (let the agent transit or be
   * aborted first). Idempotent.
   *
   * Isolation reaping: on a workflow-driven terminal the engine itself quiesces
   * (`release`) then reaps the env, but a MANUAL terminal (this path) runs with
   * no live session — the env is already DORMANT (a prior step or the human-park
   * `release` quiesced it), so this path is REAP-ONLY (no `release`). We reap by
   * the deterministic `(projectRoot, taskId)` identity. `force:false` REFUSES to
   * discard uncommitted changes (rejecting with ENVIRONMENT_DIRTY before the
   * status flips); `force:true` removes the env regardless (branches are always
   * preserved).
   */
  private async taskTerminal(params: unknown, status: "done" | "cancel"): Promise<TaskView> {
    const o = asObj(params);
    const id = reqString(o, "id");
    const force = optBool(o, "force") ?? false;
    const handle = await this.resolveHandle(reqCwd(o));
    const view = await this.getTaskView(handle.store, id);
    if (view.status === status) return view; // idempotent
    if (handle.store.sessions.hasLiveSession(id)) {
      throw new RpcError(ErrorCodes.CONFLICT, `cannot ${status} ${id}: a session is live (abort it first)`);
    }
    // Reap the isolation env BEFORE flipping the status: a dirty refusal must
    // leave the task exactly where it was so the operator can commit or retry
    // with force. No live session exists (checked above), so the env is quiescent.
    await this.reapIsolation(handle, view, force, status);
    const updated = await handle.store.setPosition(id, { status, workflow: view.workflow, step: view.step });
    // Close the claim race (review #4): the scheduler claims a task by creating a
    // `queued` session under the SESSION lock, not the task lock, so a dispatch
    // already in flight when we wrote the terminal status can land a live session
    // on a now-terminal task (which would later transit and silently overwrite
    // this terminal). The two locks are not mutually exclusive, so we detect it
    // after the fact: if a session appeared in the window, roll the position back
    // to where it was and reject — the caller retries once the session settles
    // (or aborts it explicitly, as the pre-check already requires).
    if (handle.store.sessions.hasLiveSession(id)) {
      await handle.store.setPosition(id, { status: view.status, workflow: view.workflow, step: view.step });
      throw new RpcError(ErrorCodes.CONFLICT, `cannot ${status} ${id}: a session went live (abort it first)`);
    }
    return updated;
  }

  /**
   * Reaps a task's session-free isolation env on a manual terminal (see
   * {@link taskTerminal}). Resolves the task's workflow → its `IsolationProvider`
   * and calls `reap({projectRoot, taskId}, {force})`. A dirty env with
   * `force:false` is rejected with {@link ErrorCodes.ENVIRONMENT_DIRTY} (the
   * status is untouched). Tasks with no workflow / no provider / a provider that does
   * not implement `reap` are a no-op.
   */
  private async reapIsolation(
    handle: ProjectHandle,
    view: TaskView,
    force: boolean,
    status: "done" | "cancel",
  ): Promise<void> {
    if (!view.workflow) return;
    const provider = handle.extensions.resolveWorkflow(view.workflow)?.isolation;
    if (!provider?.reap) return;
    const result = await provider.reap({ projectRoot: handle.root, taskId: view.id }, { force });
    if (result.dirty && !result.removed) {
      throw new RpcError(
        ErrorCodes.ENVIRONMENT_DIRTY,
        `cannot ${status} ${view.id}: isolation environment has uncommitted changes` +
          `${result.detail ? ` (${result.detail})` : ""}; commit them or retry with force`,
      );
    }
  }
}

// ---------------------------------------------------------------------------
// Param parsing helpers — every throw is INVALID_PARAMS so a malformed request
// gets a -32602, never an INTERNAL_ERROR.
// ---------------------------------------------------------------------------

function asObj(params: unknown): Record<string, unknown> {
  if (typeof params !== "object" || params === null || Array.isArray(params)) {
    throw new RpcError(ErrorCodes.INVALID_PARAMS, "params must be an object");
  }
  return params as Record<string, unknown>;
}

function asString(v: unknown, field: string): string {
  if (typeof v !== "string") throw new RpcError(ErrorCodes.INVALID_PARAMS, `${field} must be a string`);
  return v;
}

function reqString(o: Record<string, unknown>, field: string): string {
  const v = o[field];
  if (typeof v !== "string" || v.length === 0) {
    throw new RpcError(ErrorCodes.INVALID_PARAMS, `${field} must be a non-empty string`);
  }
  return v;
}

function optString(o: Record<string, unknown>, field: string): string | undefined {
  const v = o[field];
  if (v === undefined || v === null) return undefined;
  return asString(v, field);
}

/**
 * The `{cwd}` selector. A missing/non-string `cwd` is INVALID_PARAMS; an
 * empty/relative cwd is passed through so the project resolver maps it to
 * INVALID_PROJECT (plan §4 distinguishes the two codes).
 */
function reqCwd(o: Record<string, unknown>): string {
  const v = o.cwd;
  if (typeof v !== "string") throw new RpcError(ErrorCodes.INVALID_PARAMS, "cwd must be a string");
  return v;
}

function optBool(o: Record<string, unknown>, field: string): boolean | undefined {
  const v = o[field];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "boolean") throw new RpcError(ErrorCodes.INVALID_PARAMS, `${field} must be a boolean`);
  return v;
}

function optInt(o: Record<string, unknown>, field: string): number | undefined {
  const v = o[field];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "number" || !Number.isInteger(v)) {
    throw new RpcError(ErrorCodes.INVALID_PARAMS, `${field} must be an integer`);
  }
  return v;
}

function parseIdArray(v: unknown, field: string): string[] | undefined {
  if (v === undefined || v === null) return undefined;
  if (!Array.isArray(v) || v.some((x) => typeof x !== "string")) {
    throw new RpcError(ErrorCodes.INVALID_PARAMS, `${field} must be an array of strings`);
  }
  return v as string[];
}

function parseTaskFilter(v: unknown): TaskFilter | undefined {
  if (v === undefined || v === null) return undefined;
  const o = asObj(v);
  const filter: TaskFilter = {};
  if (o.status !== undefined) filter.status = o.status as TaskFilter["status"];
  if (o.workflow !== undefined) filter.workflow = asString(o.workflow, "filter.workflow");
  if (o.step !== undefined) filter.step = asString(o.step, "filter.step");
  if (o.blocked !== undefined) filter.blocked = Boolean(o.blocked);
  return filter;
}

function parseStepTarget(v: unknown): StepTarget | undefined {
  if (v === undefined || v === null) return undefined;
  const o = asObj(v);
  if (typeof o.step === "string") return { step: o.step };
  if (o.status === "done" || o.status === "cancel" || o.status === "human") return { status: o.status };
  throw new RpcError(ErrorCodes.INVALID_PARAMS, "to must be { step } or { status: done|cancel|human }");
}

/** Maps the store's plain `task not found` / self-block errors onto wire codes. */
async function mapTaskNotFound<T>(fn: () => Promise<T>): Promise<T> {
  try {
    return await fn();
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    if (msg.startsWith("task not found")) throw new RpcError(ErrorCodes.NOT_FOUND, msg);
    if (msg.includes("cannot block itself")) throw new RpcError(ErrorCodes.INVALID_PARAMS, msg);
    throw e;
  }
}

function errStr(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
