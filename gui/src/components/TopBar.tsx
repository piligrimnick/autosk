// TopBar — view switcher + daemon connection indicator.

import { useStore } from "@/state/store";
import type { MainView } from "@/state/types";

const VIEWS: { id: MainView; label: string }[] = [
  { id: "tasks", label: "Tasks" },
  { id: "workflows", label: "Workflows" },
  { id: "agents", label: "Agents" },
  { id: "settings", label: "Settings" },
];

export function TopBar() {
  const { state, effects } = useStore();
  const d = state.daemon;

  return (
    <header className="topbar">
      <div className="topbar-brand">autosk</div>
      <nav className="topbar-nav">
        {VIEWS.map((v) => (
          <button
            key={v.id}
            className={`tab ${state.view === v.id ? "tab-active" : ""}`}
            onClick={() => effects.setView(v.id)}
          >
            {v.label}
          </button>
        ))}
      </nav>
      <div className="topbar-status">
        <span className={`conn-dot ${d.connected ? "conn-ok" : "conn-down"}`} />
        <span className="conn-label">
          {d.connected ? "connected" : "disconnected"} · {d.mode}
        </span>
        {!d.connected && (
          <button className="btn-ghost" onClick={() => void effects.reconnect()}>
            reconnect
          </button>
        )}
      </div>
    </header>
  );
}
