/**
 * One running session — a single `onRun` invocation for one task step
 * (plan §3.2, §3.4, §3.7(4)).
 *
 * Owns the {@link AgentRunContext}, the per-session `AbortController`, the
 * transcript writer, and the settle state machine. Every way a session can end
 * funnels through exactly one of {@link commitTransit} (the agent transited),
 * {@link finalizeFailed} (threw / never transited / isolation acquire failed),
 * or {@link finalizeAborted} (aborted) — guarded by {@link settleOnce} so a race
 * (e.g. abort landing as the agent returns) finalises once.
 *
 * Lifecycle states (the queued→running transition is the dangerous one):
 *  - **queued** — the runtime exists and is reachable for steer/abort, but
 *    `onRun` has NOT started. `started` is false. A steer/followup here is
 *    rejected (`{handled:false}`); an abort here seals the session `aborted`
 *    WITHOUT ever invoking `onRun`/`onAbort`.
 *  - **running** — claimed by {@link run}: isolation is acquired (in the worker,
 *    bounded by the pool, plan §3.5), the queued→running meta write APPLIED
 *    (via the conditional `patchMetaIf`, so a concurrent abort can never be
 *    resurrected), and `started` is true.
 *  - **settled** — a terminal status was committed exactly once.
 *
 * Commit ordering rule (plan deviation #4): the scheduler scan runs CONCURRENTLY
 * with workers, so a closing session stays `running` (i.e. "live", which makes
 * the scan skip the task) until AFTER the task's new position and the isolation
 * lifecycle (quiesce / reap) are committed — only then is the meta flipped to its
 * terminal status. This is what prevents a double-dispatch and an isolation
 * acquire-vs-release race on the same task.
 */

import type {
  AgentDefinition,
  AgentRunContext,
  IsolationHandle,
  SessionMeta,
  StepTarget,
  WorkflowDefinition,
} from "@autosk/sdk";

import { buildTasksApi, buildWorkflowsApi } from "./context.ts";
import { TranscriptWriter } from "./transcript.ts";
import { execOneShot, spawnChild } from "./child.ts";
import { buildTransitContext, positionFor, validateTarget } from "./transition.ts";
import { ENGINE_COMMENT_AUTHOR, errMsg, type EngineProject, type SessionHost } from "./types.ts";

/** Everything the engine hands a session at dispatch. */
export interface SessionRuntimeInit {
  host: SessionHost;
  project: EngineProject;
  workflow: WorkflowDefinition;
  /** The workflow name the task is enrolled under. */
  workflowName: string;
  agent: AgentDefinition;
  agentName: string;
  sessionId: string;
  taskId: string;
  step: string;
}

export class SessionRuntime {
  readonly id: string;
  readonly taskId: string;

  private readonly host: SessionHost;
  private readonly project: EngineProject;
  private readonly wf: WorkflowDefinition;
  private readonly workflowName: string;
  private readonly agent: AgentDefinition;
  private readonly agentName: string;
  private readonly step: string;

  /**
   * The directory the agent runs in: the project root, or — once isolation is
   * acquired in {@link run} — the handle's path. Mutable because acquisition
   * happens in the worker (plan §3.5), not at construction.
   */
  private cwd: string;
  /** The acquired isolation handle, or `null` until {@link run} acquires one. */
  private isolation: IsolationHandle | null = null;
  /** Idempotency guard: the live env is quiesced (`release`d) at most once. */
  private isolationQuiesced = false;
  /** Idempotency guard: durable env artifacts are reaped at most once. */
  private isolationReaped = false;

  private readonly controller = new AbortController();
  private readonly transcript: TranscriptWriter;
  /** Built lazily in {@link run} once `cwd` reflects any acquired isolation. */
  private ctx: AgentRunContext | null = null;

  /** A transit has been committed (the session ended via the agent transiting). */
  private transited = false;
  /** A transit is mid-flight (guards against re-entrant `ctx.transit`). */
  private transiting = false;
  /** An abort was requested. */
  private aborted = false;
  /** `onRun` has actually started (the queued→running transition applied). */
  private started = false;
  /** The session has reached a terminal state (set once by {@link settleOnce}). */
  private settled = false;

  constructor(init: SessionRuntimeInit) {
    this.id = init.sessionId;
    this.taskId = init.taskId;
    this.host = init.host;
    this.project = init.project;
    this.wf = init.workflow;
    this.workflowName = init.workflowName;
    this.agent = init.agent;
    this.agentName = init.agentName;
    this.step = init.step;
    this.cwd = init.project.root;

    this.transcript = new TranscriptWriter(
      this.project.store.sessions,
      this.id,
      this.host.clock,
      this.host.logger,
      (entry) => this.host.emitSessionMessage(this.project, this.id, entry),
    );
  }

  /** The project root this session belongs to (defence-in-depth for routing). */
  get projectRoot(): string {
    return this.project.root;
  }

  /** Whether the session has reached a terminal state. */
  isSettled(): boolean {
    return this.settled;
  }

  // -- lifecycle (worker entry) -------------------------------------------

  /**
   * Acquires isolation, marks the session running, runs the agent's `onRun`, and
   * finalises. Called by the worker pool — so the (possibly slow) isolation
   * `acquire` is bounded by `--workers`, and a session aborted while it was still
   * queued is never run here.
   */
  async run(): Promise<void> {
    if (this.settled) return; // aborted while queued — abort() already sealed it

    // Isolation is acquired HERE, in the worker (bounded by the pool), not in the
    // scan: queued sessions hold no worktree, and a slow acquire occupies one
    // worker slot instead of stalling every project's dispatch (plan §3.5).
    if (this.wf.isolation) {
      try {
        this.isolation = await this.wf.isolation.acquire({
          projectRoot: this.project.root,
          taskId: this.taskId,
        });
        this.cwd = this.isolation.cwd;
      } catch (e) {
        await this.finalizeFailed(`isolation_acquire_failed: ${errMsg(e)}`);
        return;
      }
      // An abort could have landed during the acquire await; the session is then
      // already sealed, so quiesce the just-acquired handle and bail (no reap —
      // the env stays DORMANT for a resume, exactly like a park).
      if (this.settled) {
        await this.quiesceIsolation({ parkOnFailure: false });
        return;
      }
    }

    // The queued→running startup IO — the header `cwd` rewrite and the atomic
    // queued→running claim — is funnelled to finalizeFailed on a throw (e.g. an
    // atomicWrite hitting ENOSPC/EACCES, or a rename failing). Without this an IO
    // error here would escape to runJob, which only logs, leaving the meta
    // `queued`/`running` on disk → hasLiveSession stays true → the scheduler skips
    // the task forever (reaped only on a daemon restart). This mirrors the
    // acquire-failure and finaliser hardening (ISSUE #7 / round-5 #892).
    let meta: SessionMeta;
    let applied: boolean;
    try {
      if (this.isolation) {
        await this.project.store.sessions.setHeaderCwd(this.id, this.cwd);
      }
      // Claim the running state ATOMICALLY against a concurrent abort: only flip
      // queued→running if the session is still queued. If an abort sealed it first
      // (status=aborted), this is a no-op and we must not run `onRun`.
      const res = await this.project.store.sessions.patchMetaIf(this.id, "queued", {
        status: "running",
        started_at: this.host.clock(),
      });
      meta = res.meta;
      applied = res.applied;
    } catch (e) {
      await this.finalizeFailed(`session_start_failed: ${errMsg(e)}`);
      return;
    }
    if (!applied || this.settled) {
      await this.quiesceIsolation({ parkOnFailure: false });
      return;
    }
    this.started = true;
    this.ctx = this.buildContext();
    this.host.emitSession(this.project, meta, "status");

    let threw = false;
    let error: unknown;
    try {
      await this.agent.onRun(this.ctx);
    } catch (e) {
      threw = true;
      error = e;
    }
    await this.onRunSettled(threw, error);
  }

  /** Resolves the session once `onRun` returns or throws (unless transit/abort beat it). */
  private async onRunSettled(threw: boolean, error: unknown): Promise<void> {
    if (this.transited) return; // the agent transited — already finalised to done
    if (this.aborted) {
      await this.finalizeAborted();
      return;
    }
    await this.finalizeFailed(threw ? errMsg(error) : "agent_did_not_transit");
  }

  // -- ctx.transit --------------------------------------------------------

  /**
   * `ctx.transit` (plan §3.4): resolve + validate the target, run
   * `workflow.onTransit` (a throw propagates to the agent, which may retry with a
   * different target — it does NOT consume the session), then commit. A second
   * transit after a committed one throws.
   */
  private async transit(to: StepTarget): Promise<void> {
    if (this.transited) {
      throw new Error("transit already called for this session");
    }
    if (this.transiting) {
      throw new Error("transit already in progress for this session");
    }
    this.transiting = true;
    try {
      validateTarget(this.wf, to);
      const tctx = buildTransitContext({
        store: this.project.store,
        taskId: this.taskId,
        workflow: this.workflowName,
        leavingStep: this.step,
        author: ENGINE_COMMENT_AUTHOR,
      });
      if (this.wf.onTransit) {
        await this.wf.onTransit(tctx, to); // may throw → propagate (retryable)
      }
      // onTransit approved — claim the single settle now. If an abort landed
      // during onTransit's await, the claim fails and the transit is rejected
      // (the session already ended) rather than committing a stale transition.
      if (!this.settleOnce()) {
        throw new Error("transit: session ended before the transition could commit");
      }
      this.transited = true;
      await this.commitTransit(to);
    } finally {
      this.transiting = false;
    }
  }

  /** Commits an approved transit (task position, transcript, session meta, isolation). */
  private async commitTransit(to: StepTarget): Promise<void> {
    const from = { workflow: this.workflowName, step: this.step };

    // 1. Task position first — while the session is still `running` (live), so a
    //    concurrent scan sees the live session and skips re-dispatching.
    const pos = positionFor(this.wf, this.step, to);
    await this.project.store.setPosition(this.taskId, pos);

    // 2. Structural transcript entries.
    this.transcript.transit(to, from);
    this.transcript.sessionEnd("done");
    await this.transcript.flush();

    // 3. Isolation lifecycle, branched on the transition TARGET (plan §5.2) and
    //    driven while still live so a sibling/self transition cannot re-acquire
    //    before this completes. On the happy path a failure DOES park
    //    (parkOnFailure:true).
    //      - sibling step: keep the env RUNNING for the next step's acquire to
    //        reuse — NEITHER release NOR reap (no per-step churn).
    //      - →human: quiesce (release) only — the env stays DORMANT for a resume.
    //      - →done/cancel: quiesce (release) THEN reap(force:true) — destroy the
    //        durable artifacts (worktree dir; branch preserved).
    if ("step" in to) {
      // sibling step: nothing to do.
    } else if (to.status === "human") {
      await this.quiesceIsolation({ parkOnFailure: true });
    } else {
      await this.quiesceIsolation({ parkOnFailure: true });
      await this.reapIsolationTerminal({ parkOnFailure: true });
    }

    // 4. Now seal the session (non-live) and fan out.
    const done = await this.project.store.sessions.patchMeta(this.id, {
      status: "done",
      ended_at: this.host.clock(),
    });
    await this.host.notifyTaskChanged(this.project, this.taskId);
    this.host.emitSession(this.project, done, "done");
    this.host.kickScan();
  }

  // -- steer / followup / abort -------------------------------------------

  /**
   * Routes a `session.input` to `onSteer` / `onFollowup` (plan §3.4). Returns
   * `{ handled:false }` when the session has not started running yet (still
   * queued), has already ended, or the agent has no matching hook (the caller
   * maps that to `unsupported_by_agent`). Writes an `autosk:steer` structural
   * entry only when the message is delivered to a live run.
   */
  async input(kind: "steer" | "followup", message: string): Promise<{ handled: boolean }> {
    if (!this.started || this.settled) return { handled: false };
    const hook = kind === "steer" ? this.agent.onSteer : this.agent.onFollowup;
    if (!hook || !this.ctx) return { handled: false };
    this.transcript.steer(kind, message);
    await this.transcript.flush();
    try {
      await hook.call(this.agent, this.ctx, message);
    } catch (e) {
      this.host.logger.warn(`session ${this.id}: ${kind} hook threw (${errMsg(e)})`);
    }
    return { handled: true };
  }

  /**
   * Handles `session.abort` (plan §3.4): fire the `AbortSignal`, call `onAbort`
   * if the run had started, then finalise the session as `aborted` and park the
   * task to `human` (deviation #3: prevents the scheduler instantly re-dispatching
   * the step). A session aborted while still QUEUED is sealed without ever
   * invoking `onRun`/`onAbort` (BLOCKER #2). Idempotent — a no-op once settled.
   *
   * Return value (round-5 BLOCKER): abort ALWAYS takes effect on a live session —
   * the signal fires, the meta is sealed `aborted`, the task is parked — whether
   * or not the agent declares `onAbort` and whether or not the run had started.
   * So `handled` means "the abort acted", NOT "the agent has an onAbort hook": an
   * onAbort-less abort is never `unsupported_by_agent` (unlike steer/followup,
   * where an absent hook genuinely cannot deliver the message). Only an already
   * -settled session returns `{handled:false}`. P5 must therefore NOT translate
   * abort's `handled:false` into `unsupported_by_agent` the way it does for
   * steer/followup — here it only ever means "nothing left to abort".
   */
  async abort(): Promise<{ handled: boolean }> {
    if (this.settled) return { handled: false };
    this.aborted = true;
    this.controller.abort();
    // `onAbort` runs only when the agent declares it AND the run had started; a
    // still-queued (or mid-acquire) session is sealed WITHOUT onAbort/onRun, and
    // pump() then drops the settled runtime via isSettled().
    if (this.started && this.agent.onAbort && this.ctx) {
      try {
        await this.agent.onAbort(this.ctx);
      } catch (e) {
        this.host.logger.warn(`session ${this.id}: onAbort threw (${errMsg(e)})`);
      }
    }
    await this.finalizeAborted();
    return { handled: true };
  }

  /**
   * Detaches a session from a stopping engine WITHOUT writing terminal state:
   * marks it settled (so any late `onRun` unwind is a no-op) and fires the
   * signal so a well-behaved agent unblocks. The persisted meta is left
   * `queued`/`running` on purpose — that is what crash recovery reaps on the
   * next start.
   */
  detach(): void {
    this.settled = true;
    this.controller.abort();
  }

  // -- finalisers ---------------------------------------------------------

  private async finalizeFailed(error: string): Promise<void> {
    if (!this.settleOnce()) return;
    // Seal the session meta even if parking throws (e.g. the task was deleted
    // out-of-band): a half-finalised session left `running` is worse than a
    // failed park (ISSUE #7).
    try {
      await this.host.park(this.project, this.taskId, error);
    } catch (e) {
      this.host.logger.warn(`session ${this.id}: park after failure threw (${errMsg(e)})`);
    }
    this.transcript.error(error);
    this.transcript.sessionEnd("failed", error);
    await this.transcript.flush();
    // Park to `human` ⇒ quiesce only (no reap): the env stays DORMANT for a resume.
    await this.quiesceIsolation({ parkOnFailure: false });
    const meta = await this.project.store.sessions.patchMeta(this.id, {
      status: "failed",
      error,
      ended_at: this.host.clock(),
    });
    this.host.emitSession(this.project, meta, "error");
    this.host.kickScan();
  }

  private async finalizeAborted(): Promise<void> {
    if (!this.settleOnce()) return;
    try {
      await this.host.park(this.project, this.taskId, "aborted");
    } catch (e) {
      this.host.logger.warn(`session ${this.id}: park after abort threw (${errMsg(e)})`);
    }
    this.transcript.sessionEnd("aborted");
    await this.transcript.flush();
    // Park to `human` ⇒ quiesce only (no reap): the env stays DORMANT for a resume.
    await this.quiesceIsolation({ parkOnFailure: false });
    const meta = await this.project.store.sessions.patchMeta(this.id, {
      status: "aborted",
      ended_at: this.host.clock(),
    });
    // An aborted session reads truer as `error` than `done` for the proto-v2
    // session-event mapping (ISSUE #9a).
    this.host.emitSession(this.project, meta, "error");
    this.host.kickScan();
  }

  /** Claims the single terminal settle. Returns false if already settled. */
  private settleOnce(): boolean {
    if (this.settled) return false;
    this.settled = true;
    return true;
  }

  /**
   * Quiesces (releases) the LIVE isolation handle when the task leaves `work`
   * (a park or terminal). Idempotent — at most one quiesce per session. A
   * provider without a `release` (e.g. the worktree provider, where keeping the
   * dir IS the absence of teardown) is a no-op. On the happy-path commit a
   * release failure parks the task to `human` with the provider error (plan
   * §3.5); a finaliser passes `parkOnFailure:false` because it already parks the
   * task itself, so a release failure there must NOT double-park (ISSUE #7).
   */
  private async quiesceIsolation(opts: { parkOnFailure: boolean }): Promise<void> {
    if (!this.isolation || this.isolationQuiesced) return;
    this.isolationQuiesced = true;
    const release = this.wf.isolation?.release;
    if (!release) return; // provider has nothing to quiesce
    try {
      await release.call(this.wf.isolation, this.isolation);
    } catch (e) {
      this.host.logger.warn(`session ${this.id}: isolation release failed (${errMsg(e)})`);
      if (opts.parkOnFailure) {
        await this.host
          .park(this.project, this.taskId, `isolation_release_failed: ${errMsg(e)}`)
          .catch(() => {});
      }
    }
  }

  /**
   * Destroys the durable isolation artifacts by IDENTITY on a TERMINAL
   * transition (`done`/`cancel`). Idempotent — at most one reap per session. The
   * engine owns the close decision, so it reaps with `force:true` (the worktree
   * provider preserves the branch regardless). A provider without a `reap` is a
   * no-op. On the happy-path commit a reap failure parks the task to `human`
   * with the provider error.
   */
  private async reapIsolationTerminal(opts: { parkOnFailure: boolean }): Promise<void> {
    if (this.isolationReaped) return;
    this.isolationReaped = true;
    const reap = this.wf.isolation?.reap;
    if (!reap) return;
    try {
      await reap.call(
        this.wf.isolation,
        { projectRoot: this.project.root, taskId: this.taskId },
        { force: true },
      );
    } catch (e) {
      this.host.logger.warn(`session ${this.id}: isolation reap failed (${errMsg(e)})`);
      if (opts.parkOnFailure) {
        await this.host
          .park(this.project, this.taskId, `isolation_reap_failed: ${errMsg(e)}`)
          .catch(() => {});
      }
    }
  }

  // -- context ------------------------------------------------------------

  private buildContext(): AgentRunContext {
    const store = this.project.store;
    return {
      session: { id: this.id },
      cwd: this.cwd,
      // The canonical project root, independent of any acquired isolation handle
      // (`this.cwd` may be a worktree); lets the agent point `autosk` at the real
      // project regardless of where it runs.
      projectRoot: this.project.root,
      signal: this.controller.signal,
      tasks: buildTasksApi(store, this.taskId),
      workflows: buildWorkflowsApi(this.project.registry, this.wf, this.step),
      log: {
        message: (m) => this.transcript.message(m),
        custom: (t, d) => this.transcript.custom(t, d),
      },
      comment: (text) => store.addComment(this.taskId, { author: this.agentName, text }).then(() => {}),
      transit: (to) => this.transit(to),
      exec: (cmd, opts) =>
        execOneShot(cmd, { ...opts, defaultCwd: this.cwd, defaultSignal: this.controller.signal }),
      spawn: (cmd, opts) => spawnChild(cmd, { ...opts, defaultCwd: this.cwd, signal: this.controller.signal }),
    };
  }
}
