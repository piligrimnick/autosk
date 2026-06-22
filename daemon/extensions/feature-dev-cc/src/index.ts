/**
 * `@autosk/feature-dev-cc` — the Claude Code twin of `@autosk/feature-dev`.
 *
 * The same full feature-development cycle — `dev → review → docs → validator →
 * accept` (human) `→ cleanup → done`, with bounce-backs `review → dev` and
 * `validator → dev`, a visit-cap guard in `onTransit` (via `ctx.visits("dev")`),
 * and a per-task sandbox — but each agent step is an inline `@autosk/claude-agent`
 * role (driving `claude -p` headless stream-json) instead of an `@autosk/pi-agent`
 * role. The step key IS the agent name, `accept` is a `statusStep("human")` park
 * step, and `cleanup` is a `sandboxCleanupStep`.
 *
 * This extension registers TWO workflows:
 *  - `feature-dev-cc` — agents run in a per-task `worktreeSandbox()` (claude on
 *    the host, in the worktree);
 *  - `feature-dev-cc-docker` — agents run inside a per-task `dockerSandbox({ image })`
 *    container (a thin operator image with `claude` + toolchain). The image is
 *    `$AUTOSK_DOCKER_IMAGE` (default `autosk/claude-runtime:latest`); the harness
 *    reaches the host MCP server over `host.docker.internal`.
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
  type StepTarget,
  type TransitContext,
  type WorkflowDefinition,
} from "@autosk/sdk";
import { claudeAgent } from "@autosk/claude-agent";
import { dockerSandbox, sandboxCleanupStep, worktreeSandbox, type Sandbox } from "@autosk/sandbox";

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

/** The default docker image for `feature-dev-cc-docker`: `$AUTOSK_DOCKER_IMAGE` or a documented default. */
export function defaultDockerImage(): string {
  const env = process.env.AUTOSK_DOCKER_IMAGE;
  return env && env.trim() !== "" ? env : "autosk/claude-runtime:latest";
}

/** Options for {@link featureDevCcWorkflow} (tests inject a test sandbox double). */
export interface FeatureDevCcWorkflowOptions {
  /** Sandbox each agent step runs in (and the cleanup step tears down). Default: `worktreeSandbox()`. */
  sandbox?: Sandbox;
  /** Workflow name. Default `"feature-dev-cc"` (the docker sibling passes `"feature-dev-cc-docker"`). */
  name?: string;
  /** Workflow description override. */
  description?: string;
}

/**
 * The `feature-dev-cc` workflow definition. A factory (not a const) so tests can
 * swap the sandbox (e.g. a fake, or a temp-home worktree) and so the docker
 * sibling can reuse the exact same graph under a distinct name.
 */
export function featureDevCcWorkflow(opts: FeatureDevCcWorkflowOptions = {}): WorkflowDefinition {
  const sandbox = opts.sandbox ?? worktreeSandbox();
  return {
    name: opts.name ?? "feature-dev-cc",
    description:
      opts.description ??
      "Full feature-development cycle (Claude Code): dev → review → docs → validator → accept (human) → cleanup → done, " +
        "with review→dev and validator→dev bounce-backs and a per-task worktree sandbox.",
    firstStep: "dev",
    steps: {
      // Agents are inline: the step key (dev/review/docs/validator) IS the
      // agent name. Each is an `@autosk/claude-agent` role running its harness
      // in the per-task `sandbox`.
      dev: claudeAgent({ sandbox, firstMessage: readPrompt("dev") }),
      review: claudeAgent({ sandbox, effort: "xhigh", firstMessage: readPrompt("review") }),
      docs: claudeAgent({ sandbox, firstMessage: readPrompt("docs") }),
      validator: claudeAgent({ sandbox, firstMessage: readPrompt("validator") }),
      accept: statusStep("human"),
      // Teardown as a normal step: removes the worktree (branch preserved; for a
      // docker sandbox also rm -f any orphan container), then transits to `done`.
      cleanup: sandboxCleanupStep(sandbox),
    },
    onTransit(ctx: TransitContext, to: StepTarget): void {
      if ("step" in to && to.step === "dev" && ctx.visits("dev") >= DEV_VISIT_CAP) {
        throw new Error(
          `${opts.name ?? "feature-dev-cc"}: task ${ctx.taskId} bounced back to dev too many times ` +
            `(${ctx.visits("dev")} prior dev runs, cap ${DEV_VISIT_CAP}) — parking for a human`,
        );
      }
    },
  };
}

/** Options for {@link featureDevCcDockerWorkflow}. */
export interface FeatureDevCcDockerWorkflowOptions {
  /** Operator image with `claude` + toolchain. Default: {@link defaultDockerImage}. */
  image?: string;
}

/**
 * The `feature-dev-cc-docker` workflow: the same graph as `feature-dev-cc` but
 * each agent step runs inside a per-task `dockerSandbox({ image })` container.
 */
export function featureDevCcDockerWorkflow(opts: FeatureDevCcDockerWorkflowOptions = {}): WorkflowDefinition {
  return featureDevCcWorkflow({
    sandbox: dockerSandbox({ image: opts.image ?? defaultDockerImage() }),
    name: "feature-dev-cc-docker",
    description:
      "Full feature-development cycle (Claude Code, Docker): dev → review → docs → validator → accept (human) → cleanup → done, " +
      "each agent running inside a per-task `docker run -i --rm` container reaching the host MCP over host.docker.internal.",
  });
}

/** The extension factory: registers both the worktree and docker workflows. */
export default function featureDevCcExtension(autosk: AutoskAPI): void {
  autosk.registerWorkflow(featureDevCcWorkflow());
  autosk.registerWorkflow(featureDevCcDockerWorkflow());
}
