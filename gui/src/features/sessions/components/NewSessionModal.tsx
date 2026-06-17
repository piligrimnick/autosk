// NewSessionModal — open an interactive (taskless) chat session against a
// registered agent. The modal ONLY picks the agent (a single dropdown); the
// session opens empty and the first turn comes from the composer. No title /
// description / first-message fields (plan §7).

import { useEffect, useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { Modal } from "@/components/Modal";
import type { AgentInfo } from "@/types";

export function NewSessionModal({ cwd, onClose }: { cwd: string; onClose: () => void }) {
  const { effects, dispatch } = useStore();
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  const [agent, setAgent] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Populate the dropdown from the registry on open.
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const list = await ipc.agentList(cwd);
        if (cancelled) return;
        setAgents(list);
        if (list.length > 0) setAgent(list[0].name);
      } catch (e) {
        if (!cancelled) setErr(String((e as Error).message ?? e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [cwd]);

  const create = async () => {
    if (!agent) {
      setErr("Pick an agent.");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      const created = await ipc.sessionCreate(cwd, agent);
      // Put the session in state before selecting it (selectSession reads it to
      // decide whether to subscribe); the project subscription delivers later
      // status changes live.
      dispatch({ type: "session/upsert", session: created });
      await effects.selectSession(created.id);
      onClose();
    } catch (e) {
      setErr(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title="New session"
      onClose={onClose}
      footer={
        <button className="btn btn-primary" disabled={busy || !agent} onClick={() => void create()}>
          Start
        </button>
      }
    >
      <label className="field">
        <span className="field-label">Agent</span>
        <select
          className="select"
          autoFocus
          value={agent}
          onChange={(e) => setAgent(e.target.value)}
          disabled={agents.length === 0}
        >
          {agents.length === 0 ? (
            <option value="">(no agents registered)</option>
          ) : (
            agents.map((a) => (
              <option key={a.name} value={a.name}>
                {a.description ? `${a.name} — ${a.description}` : a.name}
              </option>
            ))
          )}
        </select>
      </label>
      {err && <div className="form-error">{err}</div>}
    </Modal>
  );
}
