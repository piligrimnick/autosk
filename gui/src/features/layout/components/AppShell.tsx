// AppShell — the top-level layout switch. On touch/phone viewports (useIsCompact)
// it mounts the compact single-pane MobileShell; otherwise the frameless
// two-panel desktop/iPad layout:
//   titlebar (drag strip) / notice / [ sidebar accordion | main ]
// The left sidebar stacks Tasks / Sessions / Workflows lazygit-style: the active
// panel grows (3:1 weight) while the others collapse. The main panel is the
// polymorphic entity view (task | session | workflow | empty) + composer.

import { useRef, type CSSProperties } from "react";
import { useStore } from "@/state/store";
import { useUiScale } from "../hooks/useUiScale";
import { useIsCompact } from "../hooks/useIsCompact";
import { NoticeBar } from "@/components/NoticeBar";
import { Titlebar } from "./Titlebar";
import { MobileShell } from "./MobileShell";
import { SidebarResizer } from "./SidebarResizer";
import { SessionsPanel } from "@/features/sessions/components/SessionsPanel";
import { CenterPanel } from "@/features/center/components/CenterPanel";
import { TasksPanel } from "@/features/tasks/components/TasksPanel";
import { WorkflowsPanel } from "@/features/workflows/components/WorkflowsPanel";
import { SettingsModal } from "@/features/settings/components/SettingsModal";

export function AppShell() {
  // All hooks run unconditionally (stable hook order) before the layout branch,
  // so a rotation/resize across the compact breakpoint never changes hook count.
  const compact = useIsCompact();
  const { state, effects } = useStore();
  // Whole-UI zoom (Cmd/Ctrl +/-/0), persisted across restarts. Inert on iOS
  // (isWebviewZoomSupported() === false), so it is safe to call in compact mode.
  useUiScale();
  const closeModal = () => effects.openModal(null);
  const panelsRef = useRef<HTMLDivElement | null>(null);
  const { sidebarCollapsed, sidebarWidth } = state.ui;
  const panelsStyle = { "--sidebar-width": `${sidebarWidth}px` } as CSSProperties;

  if (compact) return <MobileShell />;

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
    </div>
  );
}
