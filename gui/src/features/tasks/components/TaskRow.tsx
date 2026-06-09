// TaskRow — one task in the Tasks panel (redesign plan §8.5), lazy-style: a
// single line that LEADS with the status chip, then id, a flex-growing title
// (ellipsis-truncated), and the blocked flag magnetised to the right edge.
// Left-click selects the task (center → task sheet); right-click pops a NATIVE
// OS context menu at the cursor (no kebab button) — see useTaskRowMenu. Reuses
// the .task-item* classes from sidebar.css.

import { useStore } from "@/state/store";
import { StatusBadge } from "@/components/common";
import type { TaskView } from "@/types";
import { useTaskRowMenu } from "./TaskRowMenu";

export function TaskRow({ task }: { task: TaskView }) {
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
        <span className="task-status-gutter">
          <StatusBadge status={task.status} />
        </span>
        <span className="task-id">{task.id}</span>
        <span className="task-item-title">{task.title}</span>
        {task.blocked && (
          <span className="blocked-flag" title="blocked">
            ⛔
          </span>
        )}
      </li>
      {modals}
    </>
  );
}
