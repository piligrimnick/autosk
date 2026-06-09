// WorkflowView — the center body when a workflow is selected (redesign plan
// §8.3, decision #6): READ-ONLY. Header (name + isolation chips), description
// (markdown), lazy-style steps list, and the definition as pretty JSON. Actions
// are limited to isolation toggle + delete (true editing is a later phase).

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { selectedWorkflow } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { Markdown } from "@/components/Markdown";

export function WorkflowView() {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const wf = selectedWorkflow(state);
  const [busy, setBusy] = useState(false);

  if (!wf) {
    return <EmptyState title="Workflow not found" hint="It may have been deleted." />;
  }

  const toggleIsolation = async () => {
    const next = wf.isolation === "worktree" ? "none" : "worktree";
    const force = wf.non_terminal_task_count > 0;
    if (force && !confirm(`${wf.non_terminal_task_count} non-terminal task(s) use this workflow. Force isolation → ${next}?`)) {
      return;
    }
    setBusy(true);
    try {
      await ipc.workflowUpdateIsolation(cwd, wf.name, next, force);
      await effects.refreshMeta(cwd);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!confirm(`Delete workflow "${wf.name}"?`)) return;
    setBusy(true);
    try {
      await ipc.workflowDelete(cwd, wf.name);
      effects.selectWorkflow(null);
      await effects.refreshMeta(cwd);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="workflow-view">
      <div className="workflow-view-head">
        <div className="workflow-view-title-row">
          <h2 className="workflow-view-title">{wf.name}</h2>
          {wf.is_synthetic && <span className="chip">synthetic</span>}
          <span className={`chip ${wf.isolation === "worktree" ? "chip-accent" : ""}`}>{wf.isolation || "none"}</span>
        </div>
        <div className="workflow-view-meta">
          {wf.task_count} task(s) · entry: {wf.first_step || "—"}
        </div>
        {!wf.is_synthetic && (
          <div className="workflow-view-actions">
            <button className="btn btn-sm" disabled={busy} onClick={() => void toggleIsolation()}>
              isolation → {wf.isolation === "worktree" ? "none" : "worktree"}
            </button>
            <button className="btn btn-sm btn-danger" disabled={busy} onClick={() => void remove()}>
              Delete
            </button>
          </div>
        )}
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
            const targets = [...s.next_steps, ...s.next_status];
            return (
              <div key={s.id} className="workflow-step-row">
                <span className="workflow-step-name">{s.name}</span>
                <span className="workflow-step-agent">agent={s.agent_name || s.agent_id}</span>
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
