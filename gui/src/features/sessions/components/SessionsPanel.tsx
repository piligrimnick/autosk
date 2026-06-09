// SessionsPanel — the left panel: a flat list of all the active project's
// sessions (jobs), newest first (redesign plan §8.1). Cross-linking to tasks is
// deferred (decision #2).

import { useStore } from "@/state/store";
import { sessionsForProject } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { PanelHeader } from "@/features/layout/components/PanelHeader";
import { SessionRow } from "./SessionRow";

export function SessionsPanel() {
  const { state, effects } = useStore();
  const sessions = sessionsForProject(state);
  const hasProject = Boolean(state.activeProject);

  return (
    <aside className="panel panel-left">
      <PanelHeader
        title="Sessions"
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
    </aside>
  );
}
