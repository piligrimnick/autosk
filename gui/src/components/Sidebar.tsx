// Sidebar (plan §6 "Left sidebar") — projects from the registry, each
// expandable into its tasks grouped by status, with running/streaming
// indicators. Project add/remove/init.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeSlice, activityOf, groupByStatus, taskActivityMap } from "@/state/selectors";
import { StatusBadge, PriorityDot, Spinner } from "./common";
import { Modal } from "./Modal";
import { NewTaskModal } from "./NewTaskModal";

export function Sidebar() {
  const { state, effects } = useStore();
  const [adding, setAdding] = useState(false);

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <span className="sidebar-title">Projects</span>
        <div className="sidebar-actions">
          <button className="btn-ghost" title="Add / init project" onClick={() => setAdding(true)}>
            +
          </button>
          <button className="btn-ghost" title="Refresh" onClick={() => void effects.refreshProjects()}>
            ↻
          </button>
        </div>
      </div>

      {!state.projectsLoaded ? (
        <Spinner label="Loading projects…" />
      ) : state.projects.length === 0 ? (
        <div className="sidebar-empty">
          No projects registered.
          <button className="btn-link" onClick={() => setAdding(true)}>
            Add one
          </button>
        </div>
      ) : (
        <ul className="project-list">
          {state.projects.map((p) => (
            <ProjectRow key={p.root} root={p.root} name={p.name} dbPath={p.db_path} />
          ))}
        </ul>
      )}

      {adding && <AddProjectModal onClose={() => setAdding(false)} />}
    </aside>
  );
}

function ProjectRow({ root, name, dbPath }: { root: string; name: string; dbPath: string }) {
  const { state, effects } = useStore();
  const expanded = state.activeProject === root;
  const slice = expanded ? activeSlice(state) : null;

  const toggle = () => {
    void effects.selectProject(expanded ? null : root);
  };

  const onRemove = async (e: React.MouseEvent) => {
    e.stopPropagation();
    if (!confirm(`Remove ${name} from the registry? (the project's .autosk/db is left untouched)`)) return;
    try {
      await ipc.projectRemove(root);
      await effects.refreshProjects();
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    }
  };

  return (
    <li className={`project ${expanded ? "project-open" : ""}`}>
      <div className="project-row" onClick={toggle} title={dbPath}>
        <span className="project-caret">{expanded ? "▾" : "▸"}</span>
        <span className="project-name">{name}</span>
        <button className="btn-ghost project-remove" title="Remove from registry" onClick={onRemove}>
          ×
        </button>
      </div>
      {expanded && slice && <ProjectTasks loading={slice.loading} />}
    </li>
  );
}

function ProjectTasks({ loading }: { loading: boolean }) {
  const { state, effects } = useStore();
  const slice = activeSlice(state);
  const cwd = state.activeProject ?? "";
  const [creating, setCreating] = useState(false);
  const tasks = slice.taskOrder.map((id) => slice.tasks[id]).filter(Boolean);
  const groups = groupByStatus(tasks);
  // One pass over the global job map per render; look up per task below.
  const activity = taskActivityMap(state);

  const newTaskBtn = (
    <button className="btn btn-sm btn-primary new-task-btn" onClick={() => setCreating(true)}>
      + New task
    </button>
  );

  if (loading && tasks.length === 0) {
    return <Spinner label="Loading tasks…" />;
  }
  if (tasks.length === 0) {
    return (
      <div className="sidebar-empty">
        No tasks. {newTaskBtn}
        {creating && <NewTaskModal cwd={cwd} onClose={() => setCreating(false)} />}
      </div>
    );
  }

  return (
    <div className="task-groups">
      <div className="task-groups-top">{newTaskBtn}</div>
      {creating && <NewTaskModal cwd={cwd} onClose={() => setCreating(false)} />}
      {groups.map((g) => (
        <div key={g.status} className="task-group">
          <div className="task-group-head">
            <StatusBadge status={g.status} />
            <span className="task-group-count">{g.tasks.length}</span>
          </div>
          <ul className="task-list">
            {g.tasks.map((t) => {
              const act = activityOf(activity, t.id);
              const selected = state.activeTaskId === t.id;
              return (
                <li
                  key={t.id}
                  className={`task-item ${selected ? "task-selected" : ""}`}
                  onClick={() => void effects.selectTask(t.id)}
                  title={t.title}
                >
                  <div className="task-item-top">
                    <PriorityDot priority={t.priority} />
                    <span className="task-id">{t.id}</span>
                    {act.running && (
                      <span className={`run-indicator ${act.streaming ? "streaming" : ""}`} title={act.streaming ? "streaming" : "running"}>
                        ●
                      </span>
                    )}
                    {t.blocked && <span className="blocked-flag" title="blocked">⛔</span>}
                  </div>
                  <div className="task-item-title">{t.title}</div>
                  {t.step_name && (
                    <div className="task-item-step">
                      {t.workflow_name}:{t.step_name}
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        </div>
      ))}
    </div>
  );
}

function AddProjectModal({ onClose }: { onClose: () => void }) {
  const { effects } = useStore();
  const [path, setPath] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const run = async (init: boolean) => {
    if (!path.trim()) {
      setErr("Enter an absolute project path.");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      if (init) {
        await ipc.projectInit(path.trim());
      } else {
        await ipc.projectAdd(path.trim());
      }
      await effects.refreshProjects();
      onClose();
    } catch (e) {
      setErr(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title="Add project"
      onClose={onClose}
      footer={
        <>
          <button className="btn" disabled={busy} onClick={() => void run(false)}>
            Add existing
          </button>
          <button className="btn btn-primary" disabled={busy} onClick={() => void run(true)}>
            Init new
          </button>
        </>
      }
    >
      <label className="field">
        <span className="field-label">Project path (absolute)</span>
        <input
          className="input"
          autoFocus
          value={path}
          placeholder="/Users/you/code/myproject"
          onChange={(e) => setPath(e.target.value)}
        />
      </label>
      <p className="hint">
        "Add existing" registers a directory that already has <code>.autosk/db</code>. "Init new" runs migrations
        + bootstrap, then registers it.
      </p>
      {err && <div className="form-error">{err}</div>}
    </Modal>
  );
}
