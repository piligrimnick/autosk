// SessionView — the center body when a session is selected (redesign plan
// §8.3): a single-line session header (status, id, workflow:step, agent, task
// id) over the live-tailed pi-format transcript.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { selectedSession } from "@/state/selectors";
import { EmptyState, StatusBadge } from "@/components/common";
import { useConfirm } from "@/components/ConfirmDialog";
import { Transcript } from "../components/Transcript";
import { useStickToBottom } from "../useStickToBottom";
import type { SessionMeta } from "@/types";

export function SessionView() {
  const { state } = useStore();
  const session = selectedSession(state);
  // Switching to a session anchors the transcript at the newest line; while it
  // stays selected, new live events tail the bottom only when the operator is
  // already there (useStickToBottom). Hook runs unconditionally (before the
  // early return) to keep hook order stable.
  const { containerRef, onScroll } = useStickToBottom({ resetKey: session?.id ?? null });
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
          {session.kind === "interactive" ? (
            // An interactive (taskless) session has no workflow:step or task id.
            <span className="session-kind-chat">chat</span>
          ) : session.workflow ? (
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
            {session.kind !== "interactive" && (
              <span className="session-view-task-id">{session.task_id}</span>
            )}
          </span>
        </div>
      </div>
      <div className="session-view-transcript" ref={containerRef} onScroll={onScroll}>
        {lines.length === 0 ? (
          <EmptyState title="No transcript yet" hint="Waiting for the agent to produce output." />
        ) : (
          <Transcript lines={lines} />
        )}
      </div>
    </div>
  );
}

// SessionControls — the live session's End/Abort button, shown in the session
// header (the composer below is just the input).
//
//   End   — for an INTERACTIVE (chat) session: wind the agent down gracefully
//     and seal the session `done` (`session.end`).
//   Abort — for a workflow session: interrupt the live run (`session.abort`),
//     parking the task to `human`.
//
// Both need a live session, so the control is shown only for a running/queued
// session; `ok:false` just means "already settled". The action kicks off
// asynchronously, so we surface an immediate info notice (the status itself
// flips a moment later when the daemon pushes the terminal session).
function SessionControls({ session }: { session: SessionMeta }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const confirm = useConfirm();
  const [busy, setBusy] = useState(false);
  const live = session.status === "running" || session.status === "queued";
  if (!live) return null;

  const interactive = session.kind === "interactive";
  const short = session.id.slice(0, 8);

  const act = async () => {
    const ok = await confirm({
      title: interactive ? "End session" : "Abort session",
      message: interactive ? `End chat session ${short}?` : `Abort session ${short}?`,
      confirmLabel: interactive ? "End" : "Abort",
      danger: !interactive,
    });
    if (!ok) return;
    setBusy(true);
    void (async () => {
      try {
        if (interactive) {
          await ipc.sessionEnd(cwd, session.id);
          effects.setNotice({ kind: "info", text: `End sent to session ${short}.` });
        } else {
          await ipc.sessionAbort(cwd, session.id);
          effects.setNotice({ kind: "info", text: `Abort sent to session ${short}.` });
        }
      } catch (err) {
        effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
      } finally {
        setBusy(false);
      }
    })();
  };

  return (
    <button
      className={interactive ? "btn btn-sm" : "btn btn-sm btn-danger"}
      disabled={busy}
      title={interactive ? "End this chat session" : "Abort this session (queued or running)"}
      onClick={() => void act()}
    >
      {interactive ? "End" : "Abort"}
    </button>
  );
}
