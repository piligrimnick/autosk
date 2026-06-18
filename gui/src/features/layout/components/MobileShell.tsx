// MobileShell — the compact (phone) single-pane shell (iPhone compact-layout
// plan). Re-hosts the EXISTING store-driven components in a one-level-deep,
// full-screen navigation. No new state: the list-vs-detail split is derived
// straight from `selection.kind`.
//
//   list root (selection.kind === "none"):
//     MobileTopBar / NoticeBar / the one active list panel / MobileTabBar
//   detail (selection.kind !== "none"):
//     MobileTopBar (Back + title) / NoticeBar / CenterPanel (+Composer) — the
//     tab bar is hidden so the composer owns the bottom edge.
//
// The hosted list panels carry their own ＋ / ↻ actions and create/browse
// modals; the detail's in-view actions (Abort/End/Enroll) live in the view
// headers. Settings opens the same modal as on desktop (ui.modal flag); the
// other modals render from inside the re-hosted panels / ProjectSwitcher and
// ConfirmDialog from its provider, so they all work unchanged here.

import { useStore } from "@/state/store";
import { NoticeBar } from "@/components/NoticeBar";
import { TasksPanel } from "@/features/tasks/components/TasksPanel";
import { SessionsPanel } from "@/features/sessions/components/SessionsPanel";
import { WorkflowsPanel } from "@/features/workflows/components/WorkflowsPanel";
import { CenterPanel } from "@/features/center/components/CenterPanel";
import { SettingsModal } from "@/features/settings/components/SettingsModal";
import { MobileTopBar } from "./MobileTopBar";
import { MobileTabBar } from "./MobileTabBar";

/** The single list panel matching the active tab (ui.sidebarPanel). */
function ActiveList() {
  const { state } = useStore();
  switch (state.ui.sidebarPanel) {
    case "sessions":
      return <SessionsPanel />;
    case "workflows":
      return <WorkflowsPanel />;
    default:
      return <TasksPanel />;
  }
}

export function MobileShell() {
  const { state, effects } = useStore();
  const detail = state.selection.kind !== "none";
  const closeModal = () => effects.openModal(null);

  return (
    <div className={`mobile-shell${detail ? " is-detail" : " is-list"}`}>
      <MobileTopBar />
      <NoticeBar />
      <div className="mobile-body">{detail ? <CenterPanel /> : <ActiveList />}</div>
      {!detail && <MobileTabBar />}
      {state.ui.modal === "settings" && <SettingsModal onClose={closeModal} />}
    </div>
  );
}
