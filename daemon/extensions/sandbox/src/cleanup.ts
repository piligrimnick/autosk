/**
 * `sandboxCleanupStep()` — cleanup as a normal workflow step (plan §6).
 *
 * There is no `onCleanup` agent hook and no engine `reap`: a workflow author
 * wires this helper into the graph wherever teardown should happen (typically
 * `accept (human) → cleanup → done`, and any cancel path). It builds an ordinary
 * agent step whose `onRun` tears the sandbox env down via {@link Sandbox.cleanup}
 * (worktree dir removed, branch preserved; defensive container rm), comments the
 * outcome, then transits to the configured terminal (default `done`).
 *
 * It runs at the project root (so it never sits inside the dir it removes), with
 * the full host `AgentRunContext` — no sandbox of its own. Idempotent on a
 * missing env (`{ removed:false }` → a "nothing to clean up" comment).
 */

import type { AgentDefinition, StepTarget } from "@autosk/sdk";

import type { Sandbox } from "./types.ts";

/** Options for {@link sandboxCleanupStep}. */
export interface SandboxCleanupStepOptions {
  /** Where to transit after cleanup. Default `{ status: "done" }`. */
  to?: StepTarget;
  /** Remove the env even when it has uncommitted changes (branch is preserved). Default `true`. */
  force?: boolean;
}

/**
 * Builds the cleanup {@link AgentDefinition} for a sandbox. Wire it as a step:
 *
 * ```ts
 * steps: {
 *   …,
 *   accept: statusStep("human"),
 *   cleanup: sandboxCleanupStep(sandbox),
 * }
 * ```
 *
 * and route terminals through it (e.g. `accept → cleanup`, then `cleanup → done`).
 */
export function sandboxCleanupStep(sandbox: Sandbox, opts: SandboxCleanupStepOptions = {}): AgentDefinition {
  const to: StepTarget = opts.to ?? { status: "done" };
  const force = opts.force ?? true;
  return {
    async onRun(ctx): Promise<void> {
      const id = { projectRoot: ctx.projectRoot, taskId: ctx.tasks.currentId };
      const r = await sandbox.cleanup(id, { force });
      await ctx.comment(
        r.removed
          ? `cleaned up sandbox env${r.detail ? ` (${r.detail})` : ""}`
          : "no sandbox env to clean up",
      );
      await ctx.transit(to);
    },
  };
}
