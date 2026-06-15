// WorkflowsPanel — a sidebar accordion panel listing the project's workflows;
// selecting one shows its read-only definition in the main panel. Workflows are
// code now (registered by extensions), so there is no create/edit UI. Clicking
// the header (or a workflow row) expands this panel and collapses the others.

import { useState } from "react";
import { useStore } from "@/state/store";
import { activeSlice } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { PanelHeader } from "@/features/layout/components/PanelHeader";
import { WorkflowRow } from "./WorkflowRow";
import { BrowseExtensionsModal } from "./BrowseExtensionsModal";

export function WorkflowsPanel() {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const slice = activeSlice(state);
  const active = state.ui.sidebarPanel === "workflows";
  const [browsing, setBrowsing] = useState(false);

  return (
    <section className={`sidebar-panel${active ? " is-active" : ""}`}>
      <PanelHeader
        title="Workflows"
        active={active}
        onActivate={() => effects.setSidebarPanel("workflows")}
        actions={
          cwd ? (
            <>
              <button className="btn-ghost" title="Browse extensions" onClick={() => setBrowsing(true)}>
                ＋
              </button>
              <button className="btn-ghost" title="Refresh" onClick={() => void effects.refreshMeta()}>
                ↻
              </button>
            </>
          ) : null
        }
      />
      <div className="panel-body">
        {slice.workflows.length === 0 ? (
          <EmptyState title="No workflows" hint="Workflows are registered by project extensions." />
        ) : (
          <ul className="wf-list">
            {slice.workflows.map((w) => (
              <WorkflowRow key={w.name} workflow={w} />
            ))}
          </ul>
        )}
      </div>
      {browsing && cwd && <BrowseExtensionsModal cwd={cwd} onClose={() => setBrowsing(false)} />}
    </section>
  );
}
