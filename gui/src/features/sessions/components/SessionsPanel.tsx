// SessionsPanel — a sidebar accordion panel: a flat list of all the active
// project's sessions (jobs), newest first. Cross-linking to tasks is deferred.
// Clicking the header (or a row) expands this panel and collapses the others.

import { useStore } from "@/state/store";
import { sessionsForProject } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { PanelHeader } from "@/features/layout/components/PanelHeader";
import { SessionRow } from "./SessionRow";

export function SessionsPanel() {
  const { state, effects } = useStore();
  const sessions = sessionsForProject(state);
  const hasProject = Boolean(state.activeProject);
  const active = state.ui.sidebarPanel === "sessions";

  return (
    <section className={`sidebar-panel${active ? " is-active" : ""}`}>
      <PanelHeader
        title="Sessions"
        active={active}
        onActivate={() => effects.setSidebarPanel("sessions")}
        actions={
          hasProject ? (
            <button className="btn-ghost" title="Refresh" onClick={() => void effects.refreshSessions()}>
              ↻
            </button>
          ) : null
        }
      />
      <div className="panel-body">
        {!hasProject ? (
          <EmptyState title="No project" hint="Select a project to see its sessions." />
        ) : sessions.length === 0 ? (
          <EmptyState title="No sessions" hint="Runs appear here as tasks execute." />
        ) : (
          <ul className="session-list">
            {sessions.map((job) => (
              <SessionRow key={job.job_id} job={job} />
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}
