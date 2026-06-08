// TaskTimeline (plan §6 "Center task-timeline") — the task's jobs' transcripts
// concatenated chronologically, with comments and step-signals interleaved by
// timestamp, plus a live tail for the running job. assistant_* events render as
// markdown, mirroring lazy's Detail pane.

import { useEffect, useRef } from "react";
import { useStore } from "@/state/store";
import { activeTask, buildTimeline, timelineKey, type TimelineItem } from "@/state/selectors";
import type { MessageEvent } from "@/types";
import { Markdown } from "./Markdown";
import { StatusBadge, EmptyState, localTime } from "./common";

export function TaskTimeline() {
  const { state } = useStore();
  const task = activeTask(state);
  const items = buildTimeline(state, state.activeTaskId);
  const bottomRef = useRef<HTMLDivElement>(null);

  // Auto-scroll to the newest item when the transcript grows.
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [items.length, state.activeTaskId]);

  if (!task) {
    return (
      <div className="timeline timeline-empty">
        <EmptyState title="No task selected" hint="Pick a task from the sidebar to see its timeline." />
      </div>
    );
  }

  return (
    <div className="timeline">
      <div className="timeline-header">
        <div className="timeline-title-row">
          <span className="timeline-task-id">{task.id}</span>
          <StatusBadge status={task.status} />
          {task.blocked && <span className="blocked-flag">blocked</span>}
        </div>
        <h2 className="timeline-task-title">{task.title}</h2>
        {task.description && <p className="timeline-task-desc">{task.description}</p>}
      </div>

      <div className="timeline-stream">
        {items.length === 0 ? (
          <EmptyState title="No activity yet" hint="No transcripts, comments, or signals for this task." />
        ) : (
          items.map((item) => <TimelineRow key={timelineKey(item)} item={item} />)
        )}
        <div ref={bottomRef} />
      </div>
    </div>
  );
}

function TimelineRow({ item }: { item: TimelineItem }) {
  switch (item.kind) {
    case "job-start":
      return (
        <div className="tl-jobstart">
          <span className="tl-jobstart-line" />
          <span className="tl-jobstart-label">
            job {item.job.job_id.slice(0, 8)} · {item.job.workflow_name}:{item.job.step_name} · {item.job.agent_name}{" "}
            <StatusBadge status={item.job.status} />
          </span>
        </div>
      );
    case "comment":
      return (
        <div className="tl-comment">
          <div className="tl-meta">
            <span className="tl-author">💬 {item.comment.author_name || item.comment.author_id}</span>
            <span className="tl-time">{localTime(item.comment.created_at)}</span>
          </div>
          <div className="tl-comment-body">
            <Markdown text={item.comment.text} />
          </div>
        </div>
      );
    case "signal":
      return (
        <div className="tl-signal">
          <span className="tl-signal-icon">⤳</span>
          <span className="tl-signal-text">
            step <strong>{item.signal.step_name}</strong> → <strong>{item.signal.target}</strong>{" "}
            <span className="tl-muted">
              by {item.signal.agent_name || item.signal.agent_id} · {localTime(item.signal.created_at)}
            </span>
          </span>
        </div>
      );
    case "message":
      return <MessageRow event={item.event} />;
  }
}

function MessageRow({ event }: { event: MessageEvent }) {
  const kind = event.kind;
  switch (kind) {
    case "assistant_text":
      return (
        <div className="msg msg-assistant">
          <div className="msg-role">assistant</div>
          <div className="msg-body">
            <Markdown text={event.text ?? ""} />
          </div>
        </div>
      );
    case "assistant_thinking":
      return (
        <div className="msg msg-thinking">
          <div className="msg-role">thinking</div>
          <div className="msg-body msg-dim">{event.text}</div>
        </div>
      );
    case "user_text":
      return (
        <div className="msg msg-user">
          <div className="msg-role">user</div>
          <div className="msg-body">
            <Markdown text={event.text ?? ""} />
          </div>
        </div>
      );
    case "tool_call":
      return (
        <div className="msg msg-tool">
          <div className="msg-role">tool · {event.name}</div>
          {event.input != null && (
            <pre className="msg-code">{safeJson(event.input)}</pre>
          )}
        </div>
      );
    case "tool_result":
      return (
        <div className={`msg msg-toolresult ${event.is_error ? "msg-error" : ""}`}>
          <div className="msg-role">
            result{event.name ? ` · ${event.name}` : ""}
            {event.is_error ? " · error" : ""}
          </div>
          {event.text && <pre className="msg-code">{event.text}</pre>}
        </div>
      );
    default:
      // session / model_change / compaction / other — render a compact chip.
      return (
        <div className="msg msg-system">
          <span className="msg-chip">{kind}</span>
          {event.text && <span className="msg-system-text">{event.text}</span>}
        </div>
      );
  }
}

function safeJson(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
