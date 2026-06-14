// Titlebar — the full-width frameless drag strip above the 3 panels (redesign
// plan §4.3, §8). Holds the brand, the daemon connection indicator + reconnect,
// the settings gear, and (Windows only) the custom caption controls. The whole
// strip is a drag region; interactive children opt out via
// data-tauri-drag-region="false" + `-webkit-app-region: no-drag` (shell.css).
//
// The Settings button opens its modal via the store's ui.modal flag (rendered
// by AppShell); the connection dot + reconnect mirror the daemon status (plan
// §8.7, decision #7).

import { useStore } from "@/state/store";
import { isMacPlatform } from "../utils/platform";
import { WindowCaptionControls } from "./WindowCaptionControls";
import { ProjectSwitcher } from "@/features/projects/components/ProjectSwitcher";

export function Titlebar() {
  const { state, effects } = useStore();
  const d = state.daemon;

  const collapsed = state.ui.sidebarCollapsed;

  return (
    <header className={`titlebar ${isMacPlatform() ? "titlebar-mac" : ""}`} data-tauri-drag-region>
      <button
        type="button"
        className="titlebar-action"
        data-tauri-drag-region="false"
        title={collapsed ? "Show sidebar" : "Hide sidebar"}
        aria-label={collapsed ? "Show sidebar" : "Hide sidebar"}
        aria-pressed={!collapsed}
        onClick={() => effects.toggleSidebar()}
      >
        <IconSidebar collapsed={collapsed} />
      </button>
      <ProjectSwitcher />
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

function IconSidebar({ collapsed }: { collapsed: boolean }) {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.2" aria-hidden>
      <rect x="2.3" y="3.5" width="11.4" height="9" rx="1.6" />
      <line x1="6.6" y1="3.7" x2="6.6" y2="12.3" />
      {!collapsed && (
        <rect x="2.9" y="4.1" width="3.1" height="7.8" rx="0.6" fill="currentColor" stroke="none" opacity="0.5" />
      )}
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
