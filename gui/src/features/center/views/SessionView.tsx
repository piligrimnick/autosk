// SessionView — the center body when a session is selected (redesign plan
// §8.3): a single-line session header (status, id, workflow:step, agent, task
// id) over the live-tailed pi-format transcript.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { selectedSession } from "@/state/selectors";
import { EmptyState, StatusBadge } from "@/components/common";
import { Transcript } from "../components/Transcript";
import type { SessionMeta } from "@/types";

export function SessionView() {
  const { state } = useStore();
  const session = selectedSession(state);
  if (!session) {
    return <EmptyState title="Session not found" hint="It may have been removed." />;
  }
  const lines = state.transcriptBySession[session.id] ?? [];

  return (
    <div className="session-view">
      <div className="session-view-head">
        <div className="session-view-title">
          <StatusBadge status={session.status} />
          <span className="session-view-id">{session.id}</span>
          <span className="meta-sep">·</span>
          {session.workflow ? (
            <span className="session-wfstep">
              <span className="session-workflow-name">{session.workflow}</span>
              {session.step && (
                <>
                  <span className="session-step-sep">:</span>
                  <span className="session-step-name">{session.step}</span>
                </>
              )}
            </span>
          ) : (
            <span className="session-no-wf">(no-wf)</span>
          )}
          <span className="meta-sep">·</span>
          <span className="session-agent-name">{session.agent || "—"}</span>
          <span className="session-view-right">
            <SessionControls session={session} />
            <span className="session-view-task-id">{session.task_id}</span>
          </span>
        </div>
      </div>
      <div className="session-view-transcript">
        {lines.length === 0 ? (
          <EmptyState title="No transcript yet" hint="Waiting for the agent to produce output." />
        ) : (
          <Transcript lines={lines} />
        )}
      </div>
    </div>
  );
}

// SessionControls — the live session's Abort button, shown in the session
// header (the steer composer below is just the input).
//
//   Abort — interrupt the live session: the daemon asks the running agent to
//     stop (`session.abort`). It needs a live session, so it is shown only for
//     a running/queued session; `ok:false` just means "already settled".
//
// The abort kicks off asynchronously, so we surface an immediate info notice
// (the status itself flips a moment later when the daemon pushes the terminal
// session).
function SessionControls({ session }: { session: SessionMeta }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const [busy, setBusy] = useState(false);
  const live = session.status === "running" || session.status === "queued";
  if (!live) return null;

  const abort = () => {
    if (!confirm(`Abort session ${session.id.slice(0, 8)}?`)) return;
    setBusy(true);
    void (async () => {
      try {
        await ipc.sessionAbort(cwd, session.id);
        effects.setNotice({ kind: "info", text: `Abort sent to session ${session.id.slice(0, 8)}.` });
      } catch (err) {
        effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
      } finally {
        setBusy(false);
      }
    })();
  };

  return (
    <button
      className="btn btn-sm btn-danger"
      disabled={busy}
      title="Abort this session (queued or running)"
      onClick={abort}
    >
      Abort
    </button>
  );
}
