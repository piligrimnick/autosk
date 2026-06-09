// Titlebar — the full-width frameless drag strip above the 3 panels (redesign
// plan §4.3, §8). Holds the brand, the daemon connection indicator + reconnect,
// the settings gear, and (Windows only) the custom caption controls. The whole
// strip is a drag region; interactive children opt out via
// data-tauri-drag-region="false" + `-webkit-app-region: no-drag` (shell.css).
//
// The Agents + Settings buttons open their modals via the store's ui.modal flag
// (rendered by AppShell); the connection dot + reconnect mirror the daemon
// status (plan §8.7, decision #7).

import { useStore } from "@/state/store";
import { isMacPlatform } from "../utils/platform";
import { WindowCaptionControls } from "./WindowCaptionControls";

export function Titlebar() {
  const { state, effects } = useStore();
  const d = state.daemon;

  return (
    <header className={`titlebar ${isMacPlatform() ? "titlebar-mac" : ""}`} data-tauri-drag-region>
      <span className="titlebar-brand">autosk</span>
      <span className="titlebar-spacer" />

      <div className="titlebar-status" data-tauri-drag-region="false">
        <span
          className={`conn-dot ${d.connected ? "conn-ok" : "conn-down"}`}
          title={d.error ?? (d.connected ? "connected" : "disconnected")}
        />
        <span className="conn-label">
          {d.connected ? "connected" : "disconnected"} · {d.mode}
        </span>
        {!d.connected && (
          <button className="btn-ghost" onClick={() => void effects.reconnect()}>
            reconnect
          </button>
        )}
      </div>

      <button
        type="button"
        className="titlebar-action"
        data-tauri-drag-region="false"
        title="Agents"
        aria-label="Agents"
        onClick={() => effects.openModal("agents")}
      >
        <IconAgents />
      </button>
      <button
        type="button"
        className="titlebar-action"
        data-tauri-drag-region="false"
        title="Settings"
        aria-label="Settings"
        onClick={() => effects.openModal("settings")}
      >
        <IconGear />
      </button>

      <WindowCaptionControls />
    </header>
  );
}

function IconAgents() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.2" aria-hidden>
      <rect x="3" y="5" width="10" height="8" rx="2" />
      <line x1="8" y1="2.6" x2="8" y2="5" />
      <circle cx="8" cy="2.3" r="0.9" fill="currentColor" stroke="none" />
      <circle cx="6" cy="9" r="0.95" fill="currentColor" stroke="none" />
      <circle cx="10" cy="9" r="0.95" fill="currentColor" stroke="none" />
    </svg>
  );
}

function IconGear() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.2" aria-hidden>
      <circle cx="8" cy="8" r="2.2" />
      <path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M12.6 3.4l-1.4 1.4M4.8 11.2l-1.4 1.4" />
    </svg>
  );
}
