/**
 * The `singleStep` workflow factory (plan §3.3).
 *
 * Replaces v1's `single:<agent>` synthetic workflows: a built-in factory the
 * daemon materialises on demand from `task.enroll {agent}`. No persisted rows,
 * no hidden registry entries — just a one-step workflow named `single:<agent>`
 * whose only step `do` runs that agent.
 */

import type { WorkflowDefinition } from "./workflow.ts";

export function singleStep(agentName: string): WorkflowDefinition {
  return {
    name: `single:${agentName}`,
    firstStep: "do",
    steps: {
      do: { agent: agentName },
    },
  };
}
