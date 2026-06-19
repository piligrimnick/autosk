/**
 * `@autosk/feature-dev-cc` — the Claude Code twin of `@autosk/feature-dev`.
 *
 * The same full feature-development cycle — `dev → review → docs → validator →
 * accept` (human), with bounce-backs `review → dev` and `validator → dev`, a
 * visit-cap guard in `onTransit` (via `ctx.visits("dev")`), and per-task git
 * worktree isolation — but each agent step is an inline `@autosk/claude-agent`
 * role (driving `claude -p` headless stream-json) instead of an
 * `@autosk/pi-agent` role. The step key IS the agent name, and `accept` is a
 * `statusStep("human")` park step.
 *
 * Discovery: this package is NOT bootstrapped (unlike `feature-dev`). Install it
 * explicitly — from a local checkout (`autosk ext add /path/to/feature-dev-cc`)
 * or, once published, `autosk ext add npm:@autosk/feature-dev-cc`. A
 * project/global extension of the same name still wins on a collision.
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
import { claudeAgent } from "@autosk/claude-agent";
import { worktreeIsolation } from "@autosk/worktree";

/**
 * The `dev` re-entry cap: a task may enter `dev` at most this many times before
 * `onTransit` rejects the next bounce-back (the design's `max_visits` pattern).
 * `ctx.visits("dev")` counts PRIOR `dev` occupancies, so a transition INTO `dev`
 * when `visits("dev") >= 5` is the 6th entry — rejected.
 */
export const DEV_VISIT_CAP = 5;

/** Reads a role prompt (shipped under `prompts/`) to seed the agent's first message. */
function readPrompt(role: string): string {
  return readFileSync(fileURLToPath(new URL(`../prompts/${role}.md`, import.meta.url)), "utf8");
}

/** Options for {@link featureDevCcWorkflow} (tests inject a test isolation double). */
export interface FeatureDevCcWorkflowOptions {
  /** Isolation provider for the workflow. Default: `worktreeIsolation()`. */
  isolation?: IsolationProvider;
}

/**
 * The `feature-dev-cc` workflow definition. A factory (not a const) so tests can
 * swap the isolation provider (e.g. a fake, or a temp-home worktree) without
 * touching the shipped default.
 */
export function featureDevCcWorkflow(opts: FeatureDevCcWorkflowOptions = {}): WorkflowDefinition {
  return {
    name: "feature-dev-cc",
    description:
      "Full feature-development cycle (Claude Code): dev → review → docs → validator → accept (human), " +
      "with review→dev and validator→dev bounce-backs and per-task worktree isolation.",
    firstStep: "dev",
    steps: {
      // Agents are inline: the step key (dev/review/docs/validator) IS the
      // agent name, and registering the workflow registers these agents. Each
      // is an `@autosk/claude-agent` role (Claude Code's `claude -p` harness).
      dev: claudeAgent({ firstMessage: readPrompt("dev") }),
      review: claudeAgent({ effort: "xhigh", firstMessage: readPrompt("review") }),
      docs: claudeAgent({ firstMessage: readPrompt("docs") }),
      validator: claudeAgent({ firstMessage: readPrompt("validator") }),
      accept: statusStep("human"),
    },
    onTransit(ctx: TransitContext, to: StepTarget): void {
      if ("step" in to && to.step === "dev" && ctx.visits("dev") >= DEV_VISIT_CAP) {
        throw new Error(
          `feature-dev-cc: task ${ctx.taskId} bounced back to dev too many times ` +
            `(${ctx.visits("dev")} prior dev runs, cap ${DEV_VISIT_CAP}) — parking for a human`,
        );
      }
    },
    isolation: opts.isolation ?? worktreeIsolation(),
  };
}

/** The extension factory: registering the workflow registers its inline agents. */
export default function featureDevCcExtension(autosk: AutoskAPI): void {
  autosk.registerWorkflow(featureDevCcWorkflow());
}
