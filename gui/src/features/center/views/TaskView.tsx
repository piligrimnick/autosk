// TaskView — the center body when a task is selected (redesign plan §8.3,
// decision #3): the lazy-style task sheet — id/status/workflow:step/blocked
// header, title, description (markdown), and the comments thread (markdown,
// oldest → newest, editable/deletable). Transcripts live in the Session view;
// the comment box is the composer at the bottom of the center panel. The ⋯
// button at the right edge of the header row pops the same task-actions menu as
// right-clicking the task's row in the Tasks panel (useTaskRowMenu).

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeTask } from "@/state/selectors";
import { EmptyState, StatusBadge, localTime } from "@/components/common";
import { Markdown } from "@/components/Markdown";
import { useTaskRowMenu } from "@/features/tasks/components/TaskRowMenu";
import { EnrollButton } from "../components/EnrollModal";
import { useStickToBottom } from "../useStickToBottom";
import type { Comment, TaskView as TaskData } from "@/types";

export function TaskView() {
  const { state } = useStore();
  const task = activeTask(state);
  // Tasks open from the top (the title/description), unlike the session
  // transcript which opens at the tail. New comments tail the bottom only when
  // the operator is already parked there (useStickToBottom).
  const { containerRef, onScroll } = useStickToBottom({ resetKey: task?.id ?? null, resetTo: "top" });

  if (!task) {
    return <EmptyState title="Task not found" hint="It may have been removed." />;
  }
  const comments = state.extrasByTask[task.id]?.comments ?? [];

  return (
    <div className="task-view">
      <div className="task-view-head">
        <div className="task-view-title-row">
          <StatusBadge status={task.status} />
          <span className="meta-sep">·</span>
          <span className="task-view-id">{task.id}</span>
          {task.workflow && (
            <>
              <span className="meta-sep">·</span>
              <span className="task-view-wfstep">
                <span className="task-view-wf">{task.workflow}</span>
                {task.step && (
                  <>
                    <span className="task-view-step-sep">:</span>
                    <span className="task-view-step">{task.step}</span>
                  </>
                )}
              </span>
            </>
          )}
          {task.blocked && (
            <>
              <span className="meta-sep">·</span>
              <span className="blocked-flag" title="blocked">
                ⛔ blocked
              </span>
            </>
          )}
          <span className="task-view-actions">
            <EnrollButton task={task} />
            <TaskMenuButton task={task} />
          </span>
        </div>
        <h2 className="task-view-title">{task.title}</h2>
      </div>

      <div className="task-view-body" ref={containerRef} onScroll={onScroll}>
        {task.description && (
          <div className="task-desc">
            <Markdown text={task.description} />
          </div>
        )}
        <div className="task-comments">
          <div className="task-comments-head">Comments ({comments.length})</div>
          {comments.length === 0 ? (
            <div className="task-comments-empty">No comments yet.</div>
          ) : (
            comments.map((c) => <CommentItem key={c.id} task={task} comment={c} />)
          )}
        </div>
      </div>
    </div>
  );
}

// A separate component so useTaskRowMenu is called unconditionally (TaskView
// early-returns when the task is missing).
function TaskMenuButton({ task }: { task: TaskData }) {
  const { openMenu, modals } = useTaskRowMenu(task);
  return (
    <>
      <button
        className="task-view-menu-btn"
        title="Task actions"
        aria-label="Task actions"
        onClick={(e) => void openMenu(e)}
      >
        ⋯
      </button>
      {modals}
    </>
  );
}

function CommentItem({ task, comment }: { task: TaskData; comment: Comment }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(comment.text);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    if (!text.trim()) return;
    setBusy(true);
    try {
      await ipc.commentEdit(cwd, task.id, comment.id, text);
      setEditing(false);
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!confirm("Delete this comment?")) return;
    setBusy(true);
    try {
      await ipc.commentDelete(cwd, task.id, comment.id);
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="comment">
      <div className="comment-meta">
        <span className="comment-author">{comment.author}</span>
        <span className="comment-time">{localTime(comment.created_at)}</span>
        <span className="comment-actions">
          {editing ? (
            <>
              <button className="btn-ghost" disabled={busy} onClick={() => void save()}>
                save
              </button>
              <button
                className="btn-ghost"
                disabled={busy}
                onClick={() => {
                  setText(comment.text);
                  setEditing(false);
                }}
              >
                cancel
              </button>
            </>
          ) : (
            <>
              <button className="btn-ghost" disabled={busy} onClick={() => setEditing(true)}>
                edit
              </button>
              <button className="btn-ghost" disabled={busy} onClick={() => void remove()}>
                delete
              </button>
            </>
          )}
        </span>
      </div>
      <div className="comment-body">
        {editing ? (
          <textarea className="textarea mono" rows={4} value={text} onChange={(e) => setText(e.target.value)} />
        ) : (
          <Markdown text={comment.text} />
        )}
      </div>
    </div>
  );
}
