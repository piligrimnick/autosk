// TasksPanel — the top half of the right panel (redesign plan §8.5): tasks
// grouped by status, lazy-style, with a New-task action. Selecting a task shows
// its sheet in the center.

import { useState } from "react";
import { useStore } from "@/state/store";
import { activeTasks, activityOf, groupByStatus, taskActivityMap } from "@/state/selectors";
import { EmptyState, StatusBadge } from "@/components/common";
import { PanelHeader } from "@/features/layout/components/PanelHeader";
import { NewTaskModal } from "./NewTaskModal";
import { TaskRow } from "./TaskRow";

export function TasksPanel() {
  const { state } = useStore();
  const cwd = state.activeProject ?? "";
  const tasks = activeTasks(state);
  const groups = groupByStatus(tasks);
  const activity = taskActivityMap(state);
  const [creating, setCreating] = useState(false);

  return (
    <section className="panel-section panel-section-tasks">
      <PanelHeader
        title="Tasks"
        actions={
          cwd ? (
            <button className="btn-ghost" title="New task" onClick={() => setCreating(true)}>
              ＋
            </button>
          ) : null
        }
      />
      <div className="panel-body">
        {!cwd ? (
          <EmptyState title="No project" />
        ) : tasks.length === 0 ? (
          <EmptyState title="No tasks" hint="Create one with ＋." />
        ) : (
          <div className="task-groups">
            {groups.map((g) => (
              <div key={g.status} className="task-group">
                <div className="task-group-head">
                  <StatusBadge status={g.status} />
                  <span className="task-group-count">{g.tasks.length}</span>
                </div>
                <ul className="task-list">
                  {g.tasks.map((t) => (
                    <TaskRow key={t.id} task={t} activity={activityOf(activity, t.id)} />
                  ))}
                </ul>
              </div>
            ))}
          </div>
        )}
      </div>
      {creating && <NewTaskModal cwd={cwd} onClose={() => setCreating(false)} />}
    </section>
  );
}
