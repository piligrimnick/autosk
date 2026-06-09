// SessionRow — one row in the Sessions panel, modelled on the lazy-mode Jobs
// list. Two lines:
//
//   <work-time>  <job-id>  <task-id>  ……  <STATUS chip>
//                <workflow-name>:<step-name>
//
// Entity colours (job/task/workflow/step) match lazy-mode 1:1 (see the
// --job-id / --task-id / --workflow-name / --step-name tokens in base.css).
// The leading status dot is gone — the status chip on the right already carries
// that signal; running sessions keep a subtle pulse on the chip instead.

import { useStore } from "@/state/store";
import { StatusBadge, jobWorkTime } from "@/components/common";
import type { Job } from "@/types";

export function SessionRow({ job }: { job: Job }) {
  const { state, effects } = useStore();
  const selected = state.selection.kind === "session" && state.selection.jobId === job.job_id;
  const live = job.status === "running";
  const badgeCls = live ? `is-live${job.streaming ? " is-streaming" : ""}` : undefined;

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
      <div className="session-row-top">
        <span className="session-time">{jobWorkTime(job)}</span>
        <span className="session-job-id">{job.job_id}</span>
        <span className="session-task-id">{job.task_id}</span>
        <StatusBadge status={job.status} className={badgeCls} />
      </div>
      <div className="session-row-bottom">
        {job.workflow_name ? (
          <>
            <span className="session-workflow-name">{job.workflow_name}</span>
            {job.step_name && (
              <>
                <span className="session-step-sep">:</span>
                <span className="session-step-name">{job.step_name}</span>
              </>
            )}
          </>
        ) : (
          <span className="session-no-wf">(no-wf)</span>
        )}
      </div>
    </li>
  );
}
