// WorkflowsPanel — the bottom half of the right panel (redesign plan §8.6).
// Lists the project's (non-synthetic) workflows; selecting one shows its
// read-only definition in the center. Create from a JSON definition.

import { useState } from "react";
import { useStore } from "@/state/store";
import { activeSlice } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { PanelHeader } from "@/features/layout/components/PanelHeader";
import { WorkflowRow } from "./WorkflowRow";
import { CreateWorkflowModal } from "./CreateWorkflowModal";

export function WorkflowsPanel() {
  const { state } = useStore();
  const cwd = state.activeProject ?? "";
  const slice = activeSlice(state);
  const [creating, setCreating] = useState(false);

  return (
    <section className="panel-section panel-section-workflows">
      <PanelHeader
        title="workflows"
        actions={
          cwd ? (
            <button className="btn-ghost" title="Create workflow" onClick={() => setCreating(true)}>
              ＋
            </button>
          ) : null
        }
      />
      <div className="panel-body">
        {slice.workflows.length === 0 ? (
          <EmptyState title="No workflows" hint="Create one from a JSON definition." />
        ) : (
          <ul className="wf-list">
            {slice.workflows.map((w) => (
              <WorkflowRow key={w.id} workflow={w} />
            ))}
          </ul>
        )}
      </div>
      {creating && <CreateWorkflowModal cwd={cwd} onClose={() => setCreating(false)} />}
    </section>
  );
}
