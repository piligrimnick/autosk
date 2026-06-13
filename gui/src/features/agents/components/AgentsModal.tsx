// AgentsModal — a read-only list of the project's registered agents in a
// titlebar-launched modal (redesign plan §8.7). Agents are code now (registered
// by project extensions), so there is no install / uninstall UI — `AgentInfo`
// is just `{ name }`.

import { useStore } from "@/state/store";
import { activeSlice } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { Modal } from "@/components/Modal";

export function AgentsModal({ onClose }: { onClose: () => void }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const slice = activeSlice(state);

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
          </div>
          {slice.agents.length === 0 ? (
            <EmptyState title="No agents" hint="Agents are registered by project extensions." />
          ) : (
            <ul className="agent-list">
              {slice.agents.map((a) => (
                <li key={a.name} className="agent-row">
                  <span className="mono">{a.name}</span>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </Modal>
  );
}
