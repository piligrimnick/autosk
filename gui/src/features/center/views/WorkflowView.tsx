// WorkflowView — the center body when a workflow is selected (redesign plan
// §8.3, decision #6): READ-ONLY. Workflows are code now (registered by
// extensions), so this is a pure projection — header (name), description
// (markdown), lazy-style steps list (agent / human / targets), and the
// definition as pretty JSON. No create / edit / delete UI.

import { useStore } from "@/state/store";
import { selectedWorkflow } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { Markdown } from "@/components/Markdown";
import type { StepTarget } from "@/types";

function targetLabel(t: StepTarget): string {
  return "step" in t ? t.step : t.status;
}

export function WorkflowView() {
  const { state } = useStore();
  const wf = selectedWorkflow(state);

  if (!wf) {
    return <EmptyState title="Workflow not found" hint="It may no longer be registered." />;
  }

  return (
    <div className="workflow-view">
      <div className="workflow-view-head">
        <div className="workflow-view-title-row">
          <h2 className="workflow-view-title">{wf.name}</h2>
        </div>
        <div className="workflow-view-meta">entry: {wf.first_step || "—"}</div>
      </div>

      <div className="workflow-view-body">
        {wf.description && (
          <div className="workflow-desc">
            <Markdown text={wf.description} />
          </div>
        )}

        <div className="workflow-steps">
          <div className="workflow-section-head">Steps ({wf.steps.length})</div>
          {wf.steps.map((s) => {
            const targets = s.targets.map(targetLabel);
            return (
              <div key={s.name} className="workflow-step-row">
                <span className="workflow-step-name">{s.name}</span>
                <span className="workflow-step-agent">
                  {s.status === null ? "agent" : s.status}
                </span>
                <span className="workflow-step-next">next={targets.length > 0 ? targets.join(", ") : "(none)"}</span>
              </div>
            );
          })}
        </div>

        <div className="workflow-json">
          <div className="workflow-section-head">Definition (read-only)</div>
          <pre className="msg-code">{JSON.stringify(wf, null, 2)}</pre>
        </div>
      </div>
    </div>
  );
}
