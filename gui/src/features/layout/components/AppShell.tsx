// AppShell — the top-level frameless two-panel layout:
//   titlebar (drag strip) / notice / [ sidebar accordion | main ]
// The left sidebar stacks Tasks / Sessions / Workflows lazygit-style: the active
// panel grows (3:1 weight) while the others collapse. The main panel is the
// polymorphic entity view (task | session | workflow | empty) + composer.

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
        <aside className="sidebar-stack">
          <TasksPanel />
          <SessionsPanel />
          <WorkflowsPanel />
        </aside>
        <CenterPanel />
      </div>
      {state.ui.modal === "settings" && <SettingsModal onClose={closeModal} />}
      {state.ui.modal === "agents" && <AgentsModal onClose={closeModal} />}
    </div>
  );
}
