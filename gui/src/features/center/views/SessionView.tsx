// SessionView — the center body when a session is selected (redesign plan
// §8.3): a job header (id, status, workflow:step, agent, timings, attach /
// corrections / error) over the live-tailed transcript.

import { useStore } from "@/state/store";
import { selectedSessionJob } from "@/state/selectors";
import { EmptyState, StatusBadge, localTime } from "@/components/common";
import { SessionStatusDot } from "@/features/sessions/components/SessionStatusDot";
import { Transcript } from "../components/Transcript";

export function SessionView() {
  const { state } = useStore();
  const job = selectedSessionJob(state);
  if (!job) {
    return <EmptyState title="Session not found" hint="It may have been removed." />;
  }
  const messages = state.messagesByJob[job.job_id] ?? [];

  const meta = [
    job.started_at ? `started ${localTime(job.started_at)}` : null,
    job.finished_at ? `finished ${localTime(job.finished_at)}` : null,
    job.attach_count > 0 ? `attached ×${job.attach_count}` : null,
    job.max_corrections > 0 ? `corrections ${job.corrections_used}/${job.max_corrections}` : null,
    typeof job.pid === "number" ? `pid ${job.pid}` : null,
    job.error ? `error: ${job.error}` : null,
  ].filter(Boolean) as string[];

  return (
    <div className="session-view">
      <div className="session-view-head">
        <div className="session-view-title">
          <SessionStatusDot job={job} />
          <span className="mono">{job.job_id.slice(0, 8)}</span>
          <StatusBadge status={job.status} />
          <span className="session-view-meta">
            {job.workflow_name ? `${job.workflow_name}:` : ""}
            {job.step_name || "session"} · {job.agent_name || "—"}
          </span>
        </div>
        {meta.length > 0 && (
          <div className="session-view-sub">
            {meta.map((m) => (
              <span key={m}>{m}</span>
            ))}
          </div>
        )}
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
