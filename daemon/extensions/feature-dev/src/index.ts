/**
 * `@autosk/feature-dev` — the shipped reference workflow (plan §3.6, P6).
 *
 * A full feature-development cycle: `dev → review → docs → validator → accept`
 * (human) `→ cleanup → done`, with bounce-backs `review → dev` and
 * `validator → dev`, a visit-cap guard in `onTransit` (via `ctx.visits("dev")`).
 * Each agent step is an inline `@autosk/pi-agent` role seeded with that role's
 * prompt (the step key IS the agent name) and given a per-task `worktreeSandbox`;
 * `accept` is a `statusStep("human")` park step, and `cleanup` is a
 * `sandboxCleanupStep` (the human resumes the parked task into it — `autosk
 * resume <id> --to cleanup` — which tears the worktree down and transits to
 * `done`). Routing terminals through `cleanup` is what keeps a task from leaking
 * its worktree now that `done`/`cancel` are a raw status flip.
 *
 * Discovery: this package is published to npm and declared as an extension via
 * `package.json#autosk.extensions`. The daemon installs it into
 * `~/.autosk/packages/` on first run (ensureGlobalBootstrap) and lists it in
 * `~/.autosk/settings.json`, so every project can enroll into `feature-dev` with
 * no per-project files, while a project/global extension of the same name wins.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import {
  statusStep,
  type AutoskAPI,
  type StepTarget,
  type TransitContext,
  type WorkflowDefinition,
} from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import { sandboxCleanupStep, worktreeSandbox, type Sandbox } from "@autosk/sandbox";

/**
 * The `dev` re-entry cap: a task may enter `dev` at most this many times before
 * `onTransit` rejects the next bounce-back (the design's `max_visits` pattern,
 * plan §3.6). `ctx.visits("dev")` counts PRIOR `dev` occupancies, so a transition
 * INTO `dev` when `visits("dev") >= 5` is the 6th entry — rejected.
 */
export const DEV_VISIT_CAP = 5;

/** Reads a role prompt (shipped under `prompts/`) to seed the agent's first message. */
function readPrompt(role: string): string {
  return readFileSync(fileURLToPath(new URL(`../prompts/${role}.md`, import.meta.url)), "utf8");
}

/** Options for {@link featureDevWorkflow} (tests inject a test sandbox double). */
export interface FeatureDevWorkflowOptions {
  /** Sandbox each agent step runs in (and the cleanup step tears down). Default: `worktreeSandbox()`. */
  sandbox?: Sandbox;
}

/**
 * The `feature-dev` workflow definition. A factory (not a const) so tests can
 * swap the sandbox (e.g. a fake, or a temp-home worktree) without touching the
 * shipped default.
 */
export function featureDevWorkflow(opts: FeatureDevWorkflowOptions = {}): WorkflowDefinition {
  const sandbox = opts.sandbox ?? worktreeSandbox();
  return {
    name: "feature-dev",
    description:
      "Full feature-development cycle: dev → review → docs → validator → accept (human) → cleanup → done, " +
      "with review→dev and validator→dev bounce-backs and a per-task worktree sandbox.",
    firstStep: "dev",
    steps: {
      // Agents are inline: the step key (dev/review/docs/validator) IS the
      // agent name, and registering the workflow registers these agents. Each
      // runs its harness in the per-task worktree `sandbox`.
      dev: piAgent({ sandbox, firstMessage: readPrompt("dev") }),
      review: piAgent({ sandbox, thinking: "xhigh", firstMessage: readPrompt("review") }),
      docs: piAgent({ sandbox, firstMessage: readPrompt("docs") }),
      validator: piAgent({ sandbox, firstMessage: readPrompt("validator") }),
      accept: statusStep("human"),
      // Teardown as a normal step: removes the worktree (branch preserved), then
      // transits to `done`. The human resumes an accepted task into it (`autosk
      // resume <id> --to cleanup`); an agent may also route a cancel through it.
      cleanup: sandboxCleanupStep(sandbox),
    },
    onTransit(ctx: TransitContext, to: StepTarget): void {
      if ("step" in to && to.step === "dev" && ctx.visits("dev") >= DEV_VISIT_CAP) {
        throw new Error(
          `feature-dev: task ${ctx.taskId} bounced back to dev too many times ` +
            `(${ctx.visits("dev")} prior dev runs, cap ${DEV_VISIT_CAP}) — parking for a human`,
        );
      }
    },
  };
}

/** The extension factory: registering the workflow registers its inline agents. */
export default function featureDevExtension(autosk: AutoskAPI): void {
  autosk.registerWorkflow(featureDevWorkflow());
}
