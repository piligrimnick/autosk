// TaskView — the center body when a task is selected (redesign plan §8.3,
// decision #3): the lazy-style task sheet — id/status/priority/blocked header,
// title, description (markdown), and the comments thread (markdown, oldest →
// newest). Transcripts live in the Session view; the comment box is the
// composer at the bottom of the center panel.

import { useEffect, useRef } from "react";
import { useStore } from "@/state/store";
import { activeTask } from "@/state/selectors";
import { EmptyState, PriorityDot, StatusBadge, localTime } from "@/components/common";
import { Markdown } from "@/components/Markdown";
import type { Comment } from "@/types";

export function TaskView() {
  const { state } = useStore();
  const task = activeTask(state);
  const bottomRef = useRef<HTMLDivElement>(null);
  const commentCount = task ? state.extrasByTask[task.id]?.comments?.length ?? 0 : 0;

  // Sticky-tail: keep the newest comment in view as the thread grows (plan §8.3).
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: "end" });
  }, [commentCount, task?.id]);

  if (!task) {
    return <EmptyState title="Task not found" hint="It may have been removed." />;
  }
  const comments = state.extrasByTask[task.id]?.comments ?? [];

  return (
    <div className="task-view">
      <div className="task-view-head">
        <div className="task-view-title-row">
          <span className="mono task-view-id">{task.id}</span>
          <StatusBadge status={task.status} />
          <PriorityDot priority={task.priority} />
          {task.blocked && (
            <span className="blocked-flag" title="blocked">
              ⛔ blocked
            </span>
          )}
        </div>
        <h2 className="task-view-title">{task.title}</h2>
        <div className="task-view-meta">
          {task.workflow_name && (
            <span>
              {task.workflow_name}
              {task.step_name ? `:${task.step_name}` : ""}
            </span>
          )}
          {task.agent_name && <span>· {task.agent_name}</span>}
          <span>· updated {localTime(task.updated_at)}</span>
        </div>
      </div>

      <div className="task-view-body">
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
            comments.map((c) => <CommentItem key={c.id} comment={c} />)
          )}
        </div>
        <div ref={bottomRef} />
      </div>
    </div>
  );
}

function CommentItem({ comment }: { comment: Comment }) {
  return (
    <div className="comment">
      <div className="comment-meta">
        <span className="comment-author">{comment.author_name || comment.author_id}</span>
        <span className="comment-time">{localTime(comment.created_at)}</span>
      </div>
      <div className="comment-body">
        <Markdown text={comment.text} />
      </div>
    </div>
  );
}
