// SessionRow — one row in the Sessions panel, modelled on the lazy-mode
// sessions list. Two lines:
//
//   <STATUS chip>  <session-id>  ……  <task-id>
//      <work-time>  <workflow-name>:<step-name>
//
// The status chip LEADS the row from a fixed-width left gutter; the work-time
// sits in that same gutter on line 2, so the session-id and workflow:step line
// up in the column to its right and the task-id is magnetised to the right
// edge. Entity colours match lazy-mode 1:1 (see the --session-id / --task-id /
// --workflow-name / --step-name tokens in base.css). Running sessions keep a
// subtle pulse on the chip.

import { useStore } from "@/state/store";
import { StatusBadge, sessionWorkTime } from "@/components/common";
import type { SessionMeta } from "@/types";

export function SessionRow({ session }: { session: SessionMeta }) {
  const { state, effects } = useStore();
  const selected = state.selection.kind === "session" && state.selection.sessionId === session.id;
  const live = session.status === "running";
  const badgeCls = live ? "is-live" : undefined;

  return (
    <li
      className={`session-row${selected ? " is-selected" : ""}`}
      title={session.id}
      role="button"
      tabIndex={0}
      onClick={() => void effects.selectSession(session.id)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          void effects.selectSession(session.id);
        }
      }}
    >
      <div className="session-row-top">
        <span className="session-gutter">
          {/* key on the status forces React to mount a fresh <span> on a
           * status change (e.g. queued → running) rather than reusing the
           * node. Reusing it makes WebKit (WKWebView) leave the previous
           * badge's pixels composited on top of the new one once the
           * `.is-live` pulse animation promotes the node to its own layer —
           * the "two overlapping badges until you hover" glitch. */}
          <StatusBadge key={session.status} status={session.status} className={badgeCls} />
        </span>
        <span className="session-id">{session.id}</span>
        {/* An interactive (chat) session has no task id; surface the backing
         * agent on the right edge instead, mirroring SessionView's header. */}
        {session.kind === "interactive" ? (
          <span className="session-agent-name">{session.agent || "—"}</span>
        ) : (
          <span className="session-task-id">{session.task_id}</span>
        )}
      </div>
      <div className="session-row-bottom">
        <span className="session-gutter session-gutter-time">{sessionWorkTime(session)}</span>
        <span className="session-wfstep">
          {session.kind === "interactive" ? (
            // A taskless chat shows a "chat" marker where a workflow session
            // shows workflow:step (consistent with SessionView).
            <span className="session-kind-chat">chat</span>
          ) : session.workflow ? (
            <>
              <span className="session-workflow-name">{session.workflow}</span>
              {session.step && (
                <>
                  <span className="session-step-sep">:</span>
                  <span className="session-step-name">{session.step}</span>
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
