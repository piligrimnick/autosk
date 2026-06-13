/**
 * `@autosk/feature-dev` — the shipped reference workflow (plan §3.6, P6).
 *
 * A full feature-development cycle: `dev → review → docs → validator → accept`
 * (human), with bounce-backs `review → dev` and `validator → dev`, a visit-cap
 * guard in `onTransit` (via `ctx.visits("dev")`), and per-task git worktree
 * isolation. It replaces v1's `feature-dev-generic` bootstrap; each step is a
 * `@autosk/pi-agent` role seeded with that role's prompt.
 *
 * Discovery (P6 step-4 decision): this package ships as a daemon-BUNDLED
 * extension (declared via `package.json#autosk.extensions`), discovered at the
 * lowest priority so every project can enroll into `feature-dev` with no
 * per-project files, while a project/global extension of the same name wins.
 */

import { fileURLToPath } from "node:url";

import type {
  AgentDefinition,
  AutoskAPI,
  IsolationProvider,
  StepTarget,
  TransitContext,
  WorkflowDefinition,
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

/** The agent-name prefix for the shipped pi-agent roles. */
export const ROLE_PREFIX = "@autosk/pi-agent";

/** Absolute path to a role prompt file (shipped under `prompts/`). */
function promptPath(role: string): string {
  return fileURLToPath(new URL(`../prompts/${role}.md`, import.meta.url));
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
      dev: { agent: `${ROLE_PREFIX}/dev` },
      review: { agent: `${ROLE_PREFIX}/review` },
      docs: { agent: `${ROLE_PREFIX}/docs` },
      validator: { agent: `${ROLE_PREFIX}/validator` },
      accept: { human: true },
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

/** The four shipped pi-agent roles, each seeded with its role prompt. */
export function featureDevAgents(): AgentDefinition[] {
  return [
    piAgent({ name: `${ROLE_PREFIX}/dev`, firstMessageFile: promptPath("dev") }),
    piAgent({ name: `${ROLE_PREFIX}/review`, thinking: "xhigh", firstMessageFile: promptPath("review") }),
    piAgent({ name: `${ROLE_PREFIX}/docs`, firstMessageFile: promptPath("docs") }),
    piAgent({ name: `${ROLE_PREFIX}/validator`, firstMessageFile: promptPath("validator") }),
  ];
}

/** The extension factory: register the four roles, then the `feature-dev` workflow. */
export default function featureDevExtension(autosk: AutoskAPI): void {
  for (const agent of featureDevAgents()) autosk.registerAgent(agent);
  autosk.registerWorkflow(featureDevWorkflow());
}
