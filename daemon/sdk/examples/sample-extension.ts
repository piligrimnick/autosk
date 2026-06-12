/**
 * A sample autosk extension, exercised purely for typechecking (acceptance
 * criterion: a sample extension that calls registerWorkflow + registerAgent
 * against the typed AutoskAPI typechecks against the standalone @autosk/sdk
 * exports).
 *
 * Importing from the package name (not a relative path) proves the published
 * `@autosk/sdk` exports compile standalone.
 */

import type {
  AgentDefinition,
  AutoskAPI,
  WorkflowDefinition,
} from "@autosk/sdk";
import { singleStep } from "@autosk/sdk";

const devAgent: AgentDefinition = {
  name: "dev",
  async onRun(ctx) {
    // Read the current task and log a custom entry.
    const task = await ctx.tasks.current();
    ctx.log.custom("example:started", { taskId: task.id, title: task.title });

    // Write a pi message-schema entry.
    ctx.log.message({
      role: "user",
      content: "Implement the thing.",
      timestamp: Date.now(),
    });

    await ctx.comment("dev step done");
    await ctx.transit({ step: "review" });
  },
  async onSteer(ctx, message) {
    ctx.log.custom("example:steered", { message });
  },
};

const reviewAgent: AgentDefinition = {
  name: "review",
  async onRun(ctx) {
    const targets = ctx.workflows.current.targets;
    ctx.log.custom("example:review", { targets });
    await ctx.transit({ status: "done" });
  },
};

const featureDev: WorkflowDefinition = {
  name: "feature-dev",
  description: "A reference two-step workflow.",
  firstStep: "dev",
  steps: {
    dev: { agent: "dev" },
    review: { agent: "review" },
    accept: { human: true },
  },
  onTransit(ctx, to) {
    if ("step" in to && to.step === "dev" && ctx.visits("dev") >= 5) {
      throw new Error("dev bounced back too many times — park it");
    }
  },
};

export default function sampleExtension(autosk: AutoskAPI): void {
  autosk.registerAgent(devAgent);
  autosk.registerAgent(reviewAgent);
  autosk.registerWorkflow(featureDev);
  // The singleStep factory materialises a `single:<agent>` workflow.
  autosk.registerWorkflow(singleStep("dev"));
}
