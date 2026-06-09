// TasksPanel — a sidebar accordion panel: tasks grouped by status, lazy-style,
// with a New-task action. Selecting a task shows its sheet in the main panel.
// Clicking the header (or a task row) expands this panel and collapses the rest.

import { useState } from "react";
import { useStore } from "@/state/store";
import { activeTasks, activityOf, taskActivityMap, tasksByRecency } from "@/state/selectors";
import { EmptyState } from "@/components/common";
import { PanelHeader } from "@/features/layout/components/PanelHeader";
import { NewTaskModal } from "./NewTaskModal";
import { TaskRow } from "./TaskRow";

export function TasksPanel() {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const tasks = tasksByRecency(activeTasks(state));
  const activity = taskActivityMap(state);
  const active = state.ui.sidebarPanel === "tasks";
  const [creating, setCreating] = useState(false);

  return (
    <section className={`sidebar-panel${active ? " is-active" : ""}`}>
      <PanelHeader
        title="Tasks"
        active={active}
        onActivate={() => effects.setSidebarPanel("tasks")}
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
          <ul className="task-list">
            {tasks.map((t) => (
              <TaskRow key={t.id} task={t} activity={activityOf(activity, t.id)} />
            ))}
          </ul>
        )}
      </div>
      {creating && <NewTaskModal cwd={cwd} onClose={() => setCreating(false)} />}
    </section>
  );
}
