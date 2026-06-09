// WorkflowRow — one workflow in the Workflows panel (redesign plan §8.6).
// `[wt]` marks a non-synthetic worktree-isolated workflow; synthetic rows never
// carry it. Click selects the workflow (center → read-only definition).

import { useStore } from "@/state/store";
import type { Workflow } from "@/types";

export function WorkflowRow({ workflow }: { workflow: Workflow }) {
  const { state, effects } = useStore();
  const selected = state.selection.kind === "workflow" && state.selection.name === workflow.name;
  const wt = !workflow.is_synthetic && workflow.isolation === "worktree";

  return (
    <li
      className={`wf-row${selected ? " is-selected" : ""}`}
      title={workflow.name}
      role="button"
      tabIndex={0}
      onClick={() => effects.selectWorkflow(workflow.name)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          effects.selectWorkflow(workflow.name);
        }
      }}
    >
      <span className="wf-bullet">◦</span>
      <span className="wf-name">{workflow.name}</span>
      {wt && <span className="wf-wt">[wt]</span>}
    </li>
  );
}
