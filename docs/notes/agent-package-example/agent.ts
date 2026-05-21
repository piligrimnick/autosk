/**
 * Tiny example custom-runner agent.
 *
 * Demonstrates the minimum contract:
 *  - Read the task off `ctx.task`.
 *  - Do whatever you like via `ctx.cli([...])` or normal Node APIs.
 *  - Call `ctx.stepNext(...)` exactly once before resolving.
 */
import type { RunAgent } from "@autosk/agent-sdk";

const run: RunAgent = async (ctx) => {
  console.log(`example-custom-agent: working on ${ctx.task.id} (${ctx.task.title})`);

  // 1. Drop a breadcrumb in the task's comment log.
  const stamp = new Date().toISOString();
  const { code, stderr } = await ctx.cli([
    "comment",
    "add",
    ctx.task.id,
    `example-custom-agent: started at ${stamp}`,
  ]);
  if (code !== 0) {
    throw new Error(`comment add failed: ${stderr}`);
  }

  // 2. (Real agents would do work here — call an LLM, edit files, etc.)

  // 3. Close the run by picking a transition. The set of valid targets
  //    is in ctx.transitions — for a single:<agent> workflow we always
  //    have done/cancel/human.
  await ctx.stepNext("done");
};

export default run;
