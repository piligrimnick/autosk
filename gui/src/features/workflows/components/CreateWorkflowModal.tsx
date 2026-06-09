// CreateWorkflowModal — create a workflow from a JSON definition (redesign plan
// §8.6). Mirrors the legacy WorkflowsView create flow.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { Modal } from "@/components/Modal";
import { Section } from "@/components/common";

export function CreateWorkflowModal({ cwd, onClose }: { cwd: string; onClose: () => void }) {
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
