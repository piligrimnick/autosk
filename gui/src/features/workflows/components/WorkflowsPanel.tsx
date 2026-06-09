// WorkflowsPanel — a sidebar accordion panel listing the project's
// (non-synthetic) workflows; selecting one shows its read-only definition in the
// main panel. Create from a JSON definition. Clicking the header (or a workflow
// row) expands this panel and collapses the others.

import { useState } from "react";
import { useStore } from "@/state/store";
import { activeSlice } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { PanelHeader } from "@/features/layout/components/PanelHeader";
import { WorkflowRow } from "./WorkflowRow";
import { CreateWorkflowModal } from "./CreateWorkflowModal";

export function WorkflowsPanel() {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const slice = activeSlice(state);
  const active = state.ui.sidebarPanel === "workflows";
  const [creating, setCreating] = useState(false);

  return (
    <section className={`sidebar-panel${active ? " is-active" : ""}`}>
      <PanelHeader
        title="workflows"
        active={active}
        onActivate={() => effects.setSidebarPanel("workflows")}
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
