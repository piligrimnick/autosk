// AppShell — the top-level frameless 3-panel layout (redesign plan §4, §5).
//   titlebar (drag strip) / notice / [ sessions | center | tasks+workflows ]
// Phase 3 wires the Sessions panel (left) + the polymorphic center; the right
// panel (Tasks + Workflows) is filled in Phase 5.

import { useStore } from "@/state/store";
import { NoticeBar } from "@/components/NoticeBar";
import { Titlebar } from "./Titlebar";
import { SessionsPanel } from "@/features/sessions/components/SessionsPanel";
import { CenterPanel } from "@/features/center/components/CenterPanel";
import { TasksPanel } from "@/features/tasks/components/TasksPanel";
import { WorkflowsPanel } from "@/features/workflows/components/WorkflowsPanel";
import { SettingsModal } from "@/features/settings/components/SettingsModal";
import { AgentsModal } from "@/features/agents/components/AgentsModal";

export function AppShell() {
  const { state, effects } = useStore();
  const closeModal = () => effects.openModal(null);
  return (
    <div className="app-shell">
      <Titlebar />
      <NoticeBar />
      <div className="app-panels">
        <SessionsPanel />
        <CenterPanel />
        <aside className="panel panel-right">
          <TasksPanel />
          <WorkflowsPanel />
        </aside>
      </div>
      {state.ui.modal === "settings" && <SettingsModal onClose={closeModal} />}
      {state.ui.modal === "agents" && <AgentsModal onClose={closeModal} />}
    </div>
  );
}
