// SessionRow — one row in the Sessions panel, modelled on the lazy-mode Jobs
// list. Two lines:
//
//   <STATUS chip>  <job-id>  ……  <task-id>
//      <work-time>  <workflow-name>:<step-name>
//
// The status chip LEADS the row from a fixed-width left gutter; the work-time
// sits in that same gutter on line 2, so the job-id and workflow:step line up
// in the column to its right and the task-id is magnetised to the right edge.
// Entity colours (job/task/workflow/step) match lazy-mode 1:1 (see the
// --job-id / --task-id / --workflow-name / --step-name tokens in base.css).
// Running sessions keep a subtle pulse on the chip.

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
        <span className="session-gutter">
          <StatusBadge status={job.status} className={badgeCls} />
        </span>
        <span className="session-job-id">{job.job_id}</span>
        <span className="session-task-id">{job.task_id}</span>
      </div>
      <div className="session-row-bottom">
        <span className="session-gutter session-gutter-time">{jobWorkTime(job)}</span>
        <span className="session-wfstep">
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
        </span>
      </div>
    </li>
  );
}
