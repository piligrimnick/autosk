// AddProjectModal — register an existing project or init a new one (redesign
// plan §8.2). Ported from the legacy Sidebar.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { Modal } from "@/components/Modal";

export function AddProjectModal({ onClose }: { onClose: () => void }) {
  const { effects } = useStore();
  const [path, setPath] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const run = async (init: boolean) => {
    if (!path.trim()) {
      setErr("Enter an absolute project path.");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      if (init) {
        await ipc.projectInit(path.trim());
      } else {
        await ipc.projectAdd(path.trim());
      }
      await effects.refreshProjects();
      onClose();
    } catch (e) {
      setErr(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title="Add project"
      onClose={onClose}
      footer={
        <>
          <button className="btn" disabled={busy} onClick={() => void run(false)}>
            Add existing
          </button>
          <button className="btn btn-primary" disabled={busy} onClick={() => void run(true)}>
            Init new
          </button>
        </>
      }
    >
      <label className="field">
        <span className="field-label">Project path (absolute)</span>
        <input
          className="input"
          autoFocus
          value={path}
          placeholder="/Users/you/code/myproject"
          onChange={(e) => setPath(e.target.value)}
        />
      </label>
      <p className="hint">
        "Add existing" registers a directory that already has <code>.autosk/db</code>. "Init new" runs migrations
        + bootstrap, then registers it.
      </p>
      {err && <div className="form-error">{err}</div>}
    </Modal>
  );
}
