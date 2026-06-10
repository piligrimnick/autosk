// SessionView — the center body when a session is selected (redesign plan
// §8.3): a single-line job header (status, id, workflow:step, agent, task id)
// over the live-tailed transcript.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { selectedSessionJob } from "@/state/selectors";
import { EmptyState, StatusBadge } from "@/components/common";
import { Transcript } from "../components/Transcript";
import type { Job } from "@/types";

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
          <span className="session-view-right">
            <JobControls job={job} />
            <span className="session-view-task-id">{job.task_id}</span>
          </span>
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

// JobControls — the live job's abort / cancel buttons, shown in the session
// header (the steer composer below is just the input).
//
//   Cancel job — the reliable stop. The daemon fires the run's cancel token; the
//     executor's poll loop then runs the kill ladder (abort → SIGTERM → grace →
//     SIGKILL), so the job goes terminal within the grace window and the status
//     flips via the live job-event push. Queued runs are marked cancelled
//     immediately. Works for queued AND running.
//   Abort — a softer "interrupt the current turn": the daemon asks the live
//     runner to stop the turn (pi: a graceful stdin command; other runners may
//     have no registered handle → a 409). It needs a live runner, so it is
//     hidden for a queued job and may be a no-op if the agent ignores it.
//
// Both kick off asynchronously, so we surface an immediate info notice (the
// status itself flips a moment later when the daemon pushes the terminal job).
function JobControls({ job }: { job: Job }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const [busy, setBusy] = useState(false);
  const live = job.status === "running" || job.status === "queued";
  if (!live) return null;

  const run = async (fn: () => Promise<unknown>, pending: string) => {
    setBusy(true);
    try {
      await fn();
      effects.setNotice({ kind: "info", text: pending });
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  const abort = () => {
    if (!confirm("Abort the current turn?")) return;
    void run(() => ipc.jobAbort(cwd, job.job_id), `Abort sent to job ${job.job_id.slice(0, 8)}.`);
  };
  const cancel = () => {
    const verb = job.status === "queued" ? "Cancel this queued job" : "Cancel (kill) the running job";
    if (!confirm(`${verb} ${job.job_id.slice(0, 8)}?`)) return;
    void run(() => ipc.jobCancel(cwd, job.job_id), `Stopping job ${job.job_id.slice(0, 8)}\u2026`);
  };

  return (
    <>
      {job.status === "running" && (
        <button
          className="btn btn-sm btn-danger"
          disabled={busy}
          title="Interrupt the current turn (best-effort; use Cancel job to stop it)"
          onClick={abort}
        >
          Abort
        </button>
      )}
      <button
        className="btn btn-sm btn-danger"
        disabled={busy}
        title="Stop this job (queued or running)"
        onClick={cancel}
      >
        Cancel job
      </button>
    </>
  );
}
