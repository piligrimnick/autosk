// TaskRow — one task in the Tasks panel (redesign plan §8.5), lazy-style:
// priority dot, id, run/streaming indicator, blocked flag, title, workflow:step
// subline. Click selects the task (center → task sheet); the kebab opens the
// write-verb menu. Reuses the .task-item* classes from sidebar.css.

import { useState } from "react";
import { useStore } from "@/state/store";
import type { Activity } from "@/state/selectors";
import { PriorityDot } from "@/components/common";
import type { TaskView } from "@/types";
import { TaskRowMenu } from "./TaskRowMenu";

export function TaskRow({ task, activity }: { task: TaskView; activity: Activity }) {
  const { state, effects } = useStore();
  const selected = state.selection.kind === "task" && state.selection.taskId === task.id;
  const [menuAnchor, setMenuAnchor] = useState<DOMRect | null>(null);

  return (
    <li
      className={`task-item${selected ? " is-selected" : ""}`}
      title={task.title}
      onClick={() => void effects.selectTask(task.id)}
    >
      <div className="task-item-top">
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
        <button
          className="task-kebab"
          title="Actions"
          aria-label="Task actions"
          onClick={(e) => {
            e.stopPropagation();
            setMenuAnchor(e.currentTarget.getBoundingClientRect());
          }}
        >
          ⋯
        </button>
      </div>
      <div className="task-item-title">{task.title}</div>
      {task.step_name && (
        <div className="task-item-step">
          {task.workflow_name}:{task.step_name}
        </div>
      )}
      <TaskRowMenu task={task} anchor={menuAnchor} onClose={() => setMenuAnchor(null)} />
    </li>
  );
}
