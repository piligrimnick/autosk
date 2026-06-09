// SessionView — the center body when a session is selected (redesign plan
// §8.3): a single-line job header (status, id, workflow:step, agent, task id)
// over the live-tailed transcript.

import { useStore } from "@/state/store";
import { selectedSessionJob } from "@/state/selectors";
import { EmptyState, StatusBadge } from "@/components/common";
import { Transcript } from "../components/Transcript";

export function SessionView() {
  const { state } = useStore();
  const job = selectedSessionJob(state);
  if (!job) {
    return <EmptyState title="Session not found" hint="It may have been removed." />;
  }
  const messages = state.messagesByJob[job.job_id] ?? [];

  return (
    <div className="session-view">
      <div className="session-view-head">
        <div className="session-view-title">
          <StatusBadge status={job.status} />
          <span className="session-view-job-id">{job.job_id}</span>
          <span className="meta-sep">·</span>
          {job.workflow_name ? (
            <span className="session-wfstep">
              <span className="session-workflow-name">{job.workflow_name}</span>
              {job.step_name && (
                <>
                  <span className="session-step-sep">:</span>
                  <span className="session-step-name">{job.step_name}</span>
                </>
              )}
            </span>
          ) : (
            <span className="session-no-wf">(no-wf)</span>
          )}
          <span className="meta-sep">·</span>
          <span className="session-agent-name">{job.agent_name || "—"}</span>
          <span className="session-view-task-id">{job.task_id}</span>
        </div>
      </div>
      <div className="session-view-transcript">
        {messages.length === 0 ? (
          <EmptyState title="No transcript yet" hint="Waiting for the agent to produce output." />
        ) : (
          <Transcript messages={messages} />
        )}
      </div>
    </div>
  );
}
