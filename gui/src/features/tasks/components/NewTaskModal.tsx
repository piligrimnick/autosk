// NewTaskModal — task creation (lazy `CreateTask` parity); optionally enroll
// into a workflow at creation time. Relocated from src/components in the
// feature-folder reorg (redesign plan §5).

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeSlice } from "@/state/selectors";
import { Modal } from "@/components/Modal";

export function NewTaskModal({ cwd, onClose }: { cwd: string; onClose: () => void }) {
  const { state, effects } = useStore();
  const slice = activeSlice(state);
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [priority, setPriority] = useState(2);
  const [workflow, setWorkflow] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const create = async () => {
    if (!title.trim()) {
      setErr("Title is required.");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      const created = await ipc.taskCreate(cwd, {
        title: title.trim(),
        description,
        priority,
        workflow: workflow || undefined,
      });
      await effects.refreshTasks(cwd);
      await effects.selectTask(created.id);
      onClose();
    } catch (e) {
      setErr(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title="New task"
      onClose={onClose}
      footer={
        <button className="btn btn-primary" disabled={busy || !title.trim()} onClick={() => void create()}>
          Create
        </button>
      }
    >
      <label className="field">
        <span className="field-label">Title</span>
        <input className="input" autoFocus value={title} onChange={(e) => setTitle(e.target.value)} />
      </label>
      <label className="field">
        <span className="field-label">Description</span>
        <textarea className="textarea" rows={6} value={description} onChange={(e) => setDescription(e.target.value)} />
      </label>
      <div className="field-row">
        <label className="field">
          <span className="field-label">Priority</span>
          <select className="select" value={priority} onChange={(e) => setPriority(Number(e.target.value))}>
            {[0, 1, 2, 3].map((p) => (
              <option key={p} value={p}>
                P{p}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span className="field-label">Enroll (optional)</span>
          <select className="select" value={workflow} onChange={(e) => setWorkflow(e.target.value)}>
            <option value="">(none — create as new)</option>
            {slice.workflows.map((w) => (
              <option key={w.id} value={w.name}>
                {w.name}
              </option>
            ))}
          </select>
        </label>
      </div>
      {err && <div className="form-error">{err}</div>}
    </Modal>
  );
}
