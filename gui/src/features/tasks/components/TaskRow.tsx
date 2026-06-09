// TaskRow — one task in the Tasks panel (redesign plan §8.5), lazy-style: a
// single line of priority, id, run/streaming indicator, blocked flag, a
// flex-growing title (ellipsis-truncated), and a status chip magnetised to the
// right edge. Left-click selects the task (center → task sheet); right-click
// pops a NATIVE OS context menu at the cursor (no kebab button, so the status
// chip is never occluded) — see useTaskRowMenu. Reuses the .task-item* classes
// from sidebar.css.

import { useStore } from "@/state/store";
import type { Activity } from "@/state/selectors";
import { PriorityDot, StatusBadge } from "@/components/common";
import type { TaskView } from "@/types";
import { useTaskRowMenu } from "./TaskRowMenu";

export function TaskRow({ task, activity }: { task: TaskView; activity: Activity }) {
  const { state, effects } = useStore();
  const selected = state.selection.kind === "task" && state.selection.taskId === task.id;
  const { openMenu, modals } = useTaskRowMenu(task);

  return (
    <>
      <li
        className={`task-item${selected ? " is-selected" : ""}`}
        title={task.title}
        onClick={() => void effects.selectTask(task.id)}
        onContextMenu={(e) => void openMenu(e)}
      >
        <PriorityDot priority={task.priority} />
        <span className="task-id">{task.id}</span>
        {activity.running && (
          <span
            className={`run-indicator ${activity.streaming ? "streaming" : ""}`}
            title={activity.streaming ? "streaming" : "running"}
          >
            ●
          </span>
        )}
        {task.blocked && (
          <span className="blocked-flag" title="blocked">
            ⛔
          </span>
        )}
        <span className="task-item-title">{task.title}</span>
        <StatusBadge status={task.status} className="task-status" />
      </li>
      {modals}
    </>
  );
}
