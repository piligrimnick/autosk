// AgentsView (plan §6 "Secondary views") — list agents; install / uninstall.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeSlice } from "@/state/selectors";
import { EmptyState } from "./common";
import { Modal } from "./Modal";

export function AgentsView() {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const slice = activeSlice(state);
  const [installing, setInstalling] = useState(false);

  if (!cwd) {
    return <div className="view"><EmptyState title="No project selected" /></div>;
  }

  const uninstall = async (name: string) => {
    if (!confirm(`Uninstall agent "${name}"?`)) return;
    try {
      await ipc.agentUninstall(cwd, name);
      await effects.refreshMeta(cwd);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    }
  };

  return (
    <div className="view">
      <div className="view-head">
        <h2>Agents</h2>
        <div className="view-actions">
          <button className="btn" onClick={() => void effects.refreshMeta(cwd)}>↻ Refresh</button>
          <button className="btn btn-primary" onClick={() => setInstalling(true)}>+ Install</button>
        </div>
      </div>
      {slice.agents.length === 0 ? (
        <EmptyState title="No agents" />
      ) : (
        <table className="agent-table">
          <thead>
            <tr>
              <th>name</th>
              <th>source</th>
              <th>version</th>
              <th>model</th>
              <th>thinking</th>
              <th>tasks</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {slice.agents.map((a) => (
              <tr key={a.id}>
                <td className="mono">{a.name}</td>
                <td>
                  <span className="chip">{a.source}</span>
                  {a.is_human && <span className="chip chip-accent">human</span>}
                </td>
                <td>{a.version || "—"}</td>
                <td>{a.model || "—"}</td>
                <td>{a.thinking || "—"}</td>
                <td>{a.tasks_owned}</td>
                <td>
                  {/* Gate on a value the RPC actually produces. Over JSON-RPC
                      autoskd reports installed agents as source="db_only" (not
                      "installed"), so only built-in/human agents are excluded.
                      The daemon rejects uninstalling a non-package agent and the
                      error surfaces via the notice bar. */}
                  {!a.is_human && (
                    <button className="btn btn-sm btn-danger" onClick={() => void uninstall(a.name)}>
                      Uninstall
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {installing && <InstallAgentModal cwd={cwd} onClose={() => setInstalling(false)} />}
    </div>
  );
}

function InstallAgentModal({ cwd, onClose }: { cwd: string; onClose: () => void }) {
  const { effects } = useStore();
  const [name, setName] = useState("");
  const [version, setVersion] = useState("");
  const [spec, setSpec] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const install = async () => {
    if (!name.trim() && !spec.trim()) {
      setErr("Provide an agent name (or an explicit npm spec).");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await ipc.agentInstall(cwd, name.trim(), version.trim(), spec.trim());
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
      title="Install agent"
      onClose={onClose}
      footer={
        <button className="btn btn-primary" disabled={busy} onClick={() => void install()}>
          Install
        </button>
      }
    >
      <label className="field">
        <span className="field-label">Name (npm package)</span>
        <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="@scope/agent" />
      </label>
      <div className="field-row">
        <label className="field">
          <span className="field-label">Version (optional)</span>
          <input className="input" value={version} onChange={(e) => setVersion(e.target.value)} placeholder="latest" />
        </label>
        <label className="field">
          <span className="field-label">Local spec (optional)</span>
          <input className="input" value={spec} onChange={(e) => setSpec(e.target.value)} placeholder="/path/to/pkg" />
        </label>
      </div>
      {err && <div className="form-error">{err}</div>}
    </Modal>
  );
}
