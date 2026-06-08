// WorkflowsView (plan §6 "Secondary views") — create from JSON, delete,
// isolation toggle.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeSlice } from "@/state/selectors";
import type { Workflow } from "@/types";
import { EmptyState, Section } from "./common";
import { Modal } from "./Modal";

export function WorkflowsView() {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const slice = activeSlice(state);
  const [creating, setCreating] = useState(false);

  if (!cwd) {
    return <div className="view"><EmptyState title="No project selected" /></div>;
  }

  return (
    <div className="view">
      <div className="view-head">
        <h2>Workflows</h2>
        <div className="view-actions">
          <button className="btn" onClick={() => void effects.refreshMeta(cwd)}>↻ Refresh</button>
          <button className="btn btn-primary" onClick={() => setCreating(true)}>+ Create</button>
        </div>
      </div>
      {slice.workflows.length === 0 ? (
        <EmptyState title="No workflows" hint="Create one from a JSON definition." />
      ) : (
        <div className="card-grid">
          {slice.workflows.map((w) => (
            <WorkflowCard key={w.id} workflow={w} cwd={cwd} />
          ))}
        </div>
      )}
      {creating && <CreateWorkflowModal cwd={cwd} onClose={() => setCreating(false)} />}
    </div>
  );
}

function WorkflowCard({ workflow, cwd }: { workflow: Workflow; cwd: string }) {
  const { effects } = useStore();
  const [busy, setBusy] = useState(false);

  const toggleIsolation = async () => {
    const next = workflow.isolation === "worktree" ? "none" : "worktree";
    const force = workflow.non_terminal_task_count > 0;
    if (force && !confirm(`${workflow.non_terminal_task_count} non-terminal task(s) use this workflow. Force isolation → ${next}?`)) {
      return;
    }
    setBusy(true);
    try {
      await ipc.workflowUpdateIsolation(cwd, workflow.name, next, force);
      await effects.refreshMeta(cwd);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!confirm(`Delete workflow "${workflow.name}"?`)) return;
    setBusy(true);
    try {
      await ipc.workflowDelete(cwd, workflow.name);
      await effects.refreshMeta(cwd);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <div className="card-head">
        <span className="card-title">{workflow.name}</span>
        {workflow.is_synthetic && <span className="chip">synthetic</span>}
        <span className={`chip ${workflow.isolation === "worktree" ? "chip-accent" : ""}`}>
          {workflow.isolation || "none"}
        </span>
      </div>
      {workflow.description && <p className="card-desc">{workflow.description}</p>}
      <div className="card-steps">
        {workflow.steps.map((s) => (
          <span key={s.id} className="step-chip" title={`${s.agent_name || s.agent_id}`}>
            {s.name}
          </span>
        ))}
      </div>
      <div className="card-meta">
        {workflow.task_count} task(s) · entry: {workflow.first_step || "—"}
      </div>
      {!workflow.is_synthetic && (
        <div className="card-actions">
          <button className="btn btn-sm" disabled={busy} onClick={() => void toggleIsolation()}>
            isolation → {workflow.isolation === "worktree" ? "none" : "worktree"}
          </button>
          <button className="btn btn-sm btn-danger" disabled={busy} onClick={() => void remove()}>
            Delete
          </button>
        </div>
      )}
    </div>
  );
}

function CreateWorkflowModal({ cwd, onClose }: { cwd: string; onClose: () => void }) {
  const { effects } = useStore();
  const [json, setJson] = useState("");
  const [noInstall, setNoInstall] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const create = async () => {
    if (!json.trim()) {
      setErr("Paste a workflow JSON definition.");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await ipc.workflowCreate(cwd, json, noInstall);
      await effects.refreshMeta(cwd);
      onClose();
    } catch (e) {
      setErr(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title="Create workflow"
      onClose={onClose}
      footer={
        <button className="btn btn-primary" disabled={busy} onClick={() => void create()}>
          Create
        </button>
      }
    >
      <Section title="Definition (JSON)">
        <textarea
          className="textarea mono"
          rows={16}
          placeholder='{ "name": "...", "steps": [ ... ] }'
          value={json}
          onChange={(e) => setJson(e.target.value)}
        />
      </Section>
      <label className="checkbox">
        <input type="checkbox" checked={noInstall} onChange={(e) => setNoInstall(e.target.checked)} />
        Skip auto-installing referenced agents (--no-install)
      </label>
      {err && <div className="form-error">{err}</div>}
    </Modal>
  );
}
