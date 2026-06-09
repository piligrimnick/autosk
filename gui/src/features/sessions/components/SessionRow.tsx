// SessionRow — one row in the Sessions panel (redesign plan §8.1). Primary line
// is `workflow:step`, sub line is agent + relative time; an animated status dot
// reflects the job state. Click selects the session (center → transcript).

import { useStore } from "@/state/store";
import { StatusBadge, relativeTime } from "@/components/common";
import type { Job } from "@/types";
import { SessionStatusDot } from "./SessionStatusDot";

export function SessionRow({ job }: { job: Job }) {
  const { state, effects } = useStore();
  const selected = state.selection.kind === "session" && state.selection.jobId === job.job_id;
  const label = `${job.workflow_name ? `${job.workflow_name}:` : ""}${job.step_name || "session"}`;

  return (
    <li
      className={`session-row${selected ? " is-selected" : ""}`}
      title={job.job_id}
      role="button"
      tabIndex={0}
      onClick={() => void effects.selectSession(job.job_id)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          void effects.selectSession(job.job_id);
        }
      }}
    >
      <SessionStatusDot job={job} />
      <div className="session-main">
        <div className="session-row-top">
          <span className="session-label">{label}</span>
          <StatusBadge status={job.status} />
        </div>
        <div className="session-sub">
          <span className="session-agent">{job.agent_name || "—"}</span>
          <span className="session-time">{relativeTime(job.created_at)}</span>
        </div>
      </div>
    </li>
  );
}
