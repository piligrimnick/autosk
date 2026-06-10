// AppShell — the top-level frameless two-panel layout:
//   titlebar (drag strip) / notice / [ sidebar accordion | main ]
// The left sidebar stacks Tasks / Sessions / Workflows lazygit-style: the active
// panel grows (3:1 weight) while the others collapse. The main panel is the
// polymorphic entity view (task | session | workflow | empty) + composer.

import { useRef, type CSSProperties } from "react";
import { useStore } from "@/state/store";
import { useUiScale } from "../hooks/useUiScale";
import { NoticeBar } from "@/components/NoticeBar";
import { Titlebar } from "./Titlebar";
import { SidebarResizer } from "./SidebarResizer";
import { SessionsPanel } from "@/features/sessions/components/SessionsPanel";
import { CenterPanel } from "@/features/center/components/CenterPanel";
import { TasksPanel } from "@/features/tasks/components/TasksPanel";
import { WorkflowsPanel } from "@/features/workflows/components/WorkflowsPanel";
import { SettingsModal } from "@/features/settings/components/SettingsModal";
import { AgentsModal } from "@/features/agents/components/AgentsModal";

export function AppShell() {
  const { state, effects } = useStore();
  // Whole-UI zoom (Cmd/Ctrl +/-/0), persisted across restarts.
  useUiScale();
  const closeModal = () => effects.openModal(null);
  const panelsRef = useRef<HTMLDivElement | null>(null);
  const { sidebarCollapsed, sidebarWidth } = state.ui;
  const panelsStyle = { "--sidebar-width": `${sidebarWidth}px` } as CSSProperties;
  return (
    <div className="app-shell">
      <Titlebar />
      <NoticeBar />
      <div
        ref={panelsRef}
        className={`app-panels${sidebarCollapsed ? " sidebar-collapsed" : ""}`}
        style={panelsStyle}
      >
        {!sidebarCollapsed && (
          <>
            <aside className="sidebar-stack">
              <TasksPanel />
              <SessionsPanel />
              <WorkflowsPanel />
            </aside>
            <SidebarResizer containerRef={panelsRef} onCommit={effects.setSidebarWidth} />
          </>
        )}
        <CenterPanel />
      </div>
      {state.ui.modal === "settings" && <SettingsModal onClose={closeModal} />}
      {state.ui.modal === "agents" && <AgentsModal onClose={closeModal} />}
    </div>
  );
}
