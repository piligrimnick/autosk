/**
 * `@autosk/feature-dev` — the shipped reference workflow (plan §3.6, P6).
 *
 * A full feature-development cycle: `dev → review → docs → validator → accept`
 * (human), with bounce-backs `review → dev` and `validator → dev`, a visit-cap
 * guard in `onTransit` (via `ctx.visits("dev")`), and per-task git worktree
 * isolation. It replaces v1's `feature-dev-generic` bootstrap; each agent step
 * is an inline `@autosk/pi-agent` role seeded with that role's prompt (the step
 * key IS the agent name), and `accept` is a `statusStep("human")` park step.
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
  type IsolationProvider,
  type StepTarget,
  type TransitContext,
  type WorkflowDefinition,
} from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import { worktreeIsolation } from "@autosk/worktree";

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

/** Options for {@link featureDevWorkflow} (tests inject a test isolation double). */
export interface FeatureDevWorkflowOptions {
  /** Isolation provider for the workflow. Default: `worktreeIsolation()`. */
  isolation?: IsolationProvider;
}

/**
 * The `feature-dev` workflow definition. A factory (not a const) so tests can
 * swap the isolation provider (e.g. a fake, or a temp-home worktree) without
 * touching the shipped default.
 */
export function featureDevWorkflow(opts: FeatureDevWorkflowOptions = {}): WorkflowDefinition {
  return {
    name: "feature-dev",
    description:
      "Full feature-development cycle: dev → review → docs → validator → accept (human), " +
      "with review→dev and validator→dev bounce-backs and per-task worktree isolation.",
    firstStep: "dev",
    steps: {
      // Agents are inline: the step key (dev/review/docs/validator) IS the
      // agent name, and registering the workflow registers these agents.
      dev: piAgent({ firstMessage: readPrompt("dev") }),
      review: piAgent({ thinking: "xhigh", firstMessage: readPrompt("review") }),
      docs: piAgent({ firstMessage: readPrompt("docs") }),
      validator: piAgent({ firstMessage: readPrompt("validator") }),
      accept: statusStep("human"),
    },
    onTransit(ctx: TransitContext, to: StepTarget): void {
      if ("step" in to && to.step === "dev" && ctx.visits("dev") >= DEV_VISIT_CAP) {
        throw new Error(
          `feature-dev: task ${ctx.taskId} bounced back to dev too many times ` +
            `(${ctx.visits("dev")} prior dev runs, cap ${DEV_VISIT_CAP}) — parking for a human`,
        );
      }
    },
    isolation: opts.isolation ?? worktreeIsolation(),
  };
}

/** The extension factory: registering the workflow registers its inline agents. */
export default function featureDevExtension(autosk: AutoskAPI): void {
  autosk.registerWorkflow(featureDevWorkflow());
}
