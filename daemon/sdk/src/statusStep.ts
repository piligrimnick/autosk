/**
 * The `statusStep` helper (plan §3.3).
 *
 * Builds a terminal/park step value for a workflow's `steps` map. Entering such
 * a step does not schedule an agent; the engine moves the task to that status:
 * `human` parks it (resumable via `task.resume`), while `done`/`cancel` close
 * it (recording the step the task ended on).
 *
 * ```ts
 * autosk.registerWorkflow({
 *   name: "feature-dev",
 *   firstStep: "dev",
 *   steps: {
 *     dev:    piAgent({ firstMessageFile: ".../dev.md" }),
 *     accept: statusStep("human"),
 *   },
 * });
 * ```
 */

import type { StatusStep } from "./workflow.ts";

export function statusStep(status: "done" | "cancel" | "human"): StatusStep {
  return { status };
}
