// MobileTopBar — the compact replacement for the desktop Titlebar (iPhone
// compact-layout plan). Two modes, chosen by `selection.kind`:
//   - list   → ProjectSwitcher (left) · connection dot + Settings gear (right);
//   - detail → ‹ Back (clearSelection) + entity title (left) · Settings gear.
// Deliberately omits the desktop-only chrome: no sidebar-collapse toggle, no
// Windows caption controls, no macOS traffic-light inset, and no drag region
// (all meaningless on iOS).

import { useStore } from "@/state/store";
import type { AppState } from "@/state/types";
import { activeTask, selectedSession, selectedWorkflow } from "@/state/selectors";
import { ProjectSwitcher } from "@/features/projects/components/ProjectSwitcher";

export function MobileTopBar() {
  const { state, effects } = useStore();
  const sel = state.selection;
  const detail = sel.kind !== "none";
  const d = state.daemon;

  const settingsBtn = (
    <button
      type="button"
      className="mobile-icon-btn"
      title="Settings"
      aria-label="Settings"
      onClick={() => effects.openModal("settings")}
    >
      <IconGear />
    </button>
  );

  if (detail) {
    return (
      <header className="mobile-topbar mobile-topbar-detail">
        <button
          type="button"
          className="mobile-icon-btn mobile-back"
          title="Back"
          aria-label="Back"
          onClick={() => effects.clearSelection()}
        >
          <IconChevronLeft />
          <span className="mobile-back-label">Back</span>
        </button>
        <span className="mobile-topbar-title">{detailTitle(state)}</span>
        {settingsBtn}
      </header>
    );
  }

  return (
    <header className="mobile-topbar mobile-topbar-list">
      <ProjectSwitcher />
      <span className="mobile-topbar-spacer" />
      <span
        className={`conn-dot ${d.connected ? "conn-ok" : "conn-down"}`}
        title={d.error ?? (d.connected ? "connected" : "disconnected")}
      />
      {settingsBtn}
    </header>
  );
}

/** The pushed entity's title for the detail top bar (falls back to its id). */
function detailTitle(state: AppState): string {
  const sel = state.selection;
  if (sel.kind === "task") return activeTask(state)?.title || sel.taskId;
  if (sel.kind === "session") return selectedSession(state)?.id || sel.sessionId;
  if (sel.kind === "workflow") return selectedWorkflow(state)?.name || sel.name;
  return "";
}

function IconGear() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.2" aria-hidden>
      <circle cx="8" cy="8" r="2.2" />
      <path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M12.6 3.4l-1.4 1.4M4.8 11.2l-1.4 1.4" />
    </svg>
  );
}

function IconChevronLeft() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden>
      <path d="M10 3.5L5.5 8l4.5 4.5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
