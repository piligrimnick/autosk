// AgentsModal — list / install / uninstall agents in a titlebar-launched modal
// (redesign plan §8.7). Ported from the legacy AgentsView; install uses an
// inline form rather than a nested modal.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeSlice } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { Modal } from "@/components/Modal";

export function AgentsModal({ onClose }: { onClose: () => void }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const slice = activeSlice(state);
  const [installing, setInstalling] = useState(false);

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
    <Modal title="Agents" onClose={onClose}>
      {!cwd ? (
        <EmptyState title="No project selected" />
      ) : (
        <>
          <div className="view-actions agents-actions">
            <button className="btn" onClick={() => void effects.refreshMeta(cwd)}>
              ↻ Refresh
            </button>
            <button className="btn btn-primary" onClick={() => setInstalling((v) => !v)}>
              {installing ? "Cancel" : "+ Install"}
            </button>
          </div>
          {installing && <InstallForm cwd={cwd} onDone={() => setInstalling(false)} />}
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
                  <th />
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
        </>
      )}
    </Modal>
  );
}

function InstallForm({ cwd, onDone }: { cwd: string; onDone: () => void }) {
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
      onDone();
    } catch (e) {
      setErr(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="agents-install">
      <div className="field-row">
        <label className="field">
          <span className="field-label">Name (npm package)</span>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="@scope/agent" />
        </label>
        <label className="field">
          <span className="field-label">Version</span>
          <input className="input" value={version} onChange={(e) => setVersion(e.target.value)} placeholder="latest" />
        </label>
        <label className="field">
          <span className="field-label">Local spec</span>
          <input className="input" value={spec} onChange={(e) => setSpec(e.target.value)} placeholder="/path/to/pkg" />
        </label>
      </div>
      {err && <div className="form-error">{err}</div>}
      <div className="view-actions">
        <button className="btn btn-primary" disabled={busy} onClick={() => void install()}>
          Install
        </button>
      </div>
    </div>
  );
}
