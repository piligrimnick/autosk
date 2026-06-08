// RightPanel (plan §6 "Right panel") — workflow steps (graph + per-step agent +
// next= targets), task metadata + visit counters, worktree path/branch. Also
// the home of the remaining task write verbs (status/priority/title/desc/
// metadata/block) so every lazy write is reachable from the GUI.

import { useMemo, useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeSlice } from "@/state/selectors";
import type { TaskView, Workflow } from "@/types";
import { Section, StatusBadge, localTime } from "./common";
import { Modal } from "./Modal";

export function RightPanel({ task }: { task: TaskView | null }) {
  const { state } = useStore();
  const slice = activeSlice(state);
  if (!task) {
    return <aside className="right-panel right-empty" />;
  }
  const workflow = slice.workflows.find((w) => w.id === task.workflow_id) ?? null;

  return (
    <aside className="right-panel">
      <TaskActions task={task} />
      <TaskMeta task={task} />
      {workflow && <WorkflowGraph workflow={workflow} currentStepId={task.current_step_id} />}
      {workflow && <VisitCounters task={task} workflow={workflow} />}
      <WorktreeInfo task={task} isolation={workflow?.isolation ?? "none"} projectRoot={state.activeProject ?? ""} />
    </aside>
  );
}

function TaskActions({ task }: { task: TaskView }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const [editing, setEditing] = useState(false);
  const [editingMeta, setEditingMeta] = useState(false);
  const [busy, setBusy] = useState(false);

  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    try {
      await fn();
      await effects.refreshTask(task.id);
      await effects.refreshTasks();
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="task-actions">
      <div className="task-actions-row">
        <label className="composer-inline">
          priority
          <select
            className="select"
            value={task.priority}
            disabled={busy}
            onChange={(e) => void run(() => ipc.taskSetPriority(cwd, task.id, Number(e.target.value)))}
          >
            {[0, 1, 2, 3].map((p) => (
              <option key={p} value={p}>
                P{p}
              </option>
            ))}
          </select>
        </label>
        <button className="btn" disabled={busy} onClick={() => setEditing(true)}>
          Edit
        </button>
        <button className="btn" disabled={busy} onClick={() => setEditingMeta(true)}>
          Metadata
        </button>
      </div>
      <div className="task-actions-row">
        {task.status !== "done" && (
          <button className="btn" disabled={busy} onClick={() => void run(() => ipc.taskDone(cwd, task.id))}>
            Done
          </button>
        )}
        {task.status !== "cancel" && (
          <button className="btn" disabled={busy} onClick={() => void run(() => ipc.taskCancel(cwd, task.id))}>
            Cancel
          </button>
        )}
        <BlockControl task={task} disabled={busy} onRun={run} cwd={cwd} />
      </div>
      {editing && <EditTaskModal task={task} cwd={cwd} onClose={() => setEditing(false)} onSaved={() => void run(async () => {})} />}
      {editingMeta && <MetadataModal task={task} cwd={cwd} onClose={() => setEditingMeta(false)} onSaved={() => void run(async () => {})} />}
    </div>
  );
}

function BlockControl({
  task,
  cwd,
  disabled,
  onRun,
}: {
  task: TaskView;
  cwd: string;
  disabled: boolean;
  onRun: (fn: () => Promise<unknown>) => Promise<void>;
}) {
  const [blocker, setBlocker] = useState("");
  return (
    <div className="block-control">
      <input
        className="input input-sm"
        placeholder="blocker id…"
        value={blocker}
        disabled={disabled}
        onChange={(e) => setBlocker(e.target.value)}
      />
      <button
        className="btn"
        disabled={disabled || !blocker.trim()}
        onClick={() => {
          const id = blocker.trim();
          setBlocker("");
          void onRun(() => ipc.taskBlock(cwd, task.id, [id]));
        }}
      >
        Block
      </button>
    </div>
  );
}

function TaskMeta({ task }: { task: TaskView }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const unblock = async (id: string) => {
    try {
      await ipc.taskUnblock(cwd, task.id, [id]);
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    }
  };
  return (
    <Section title="Task">
      <dl className="meta-list">
        <Row k="id" v={task.id} />
        <Row k="status" v={<StatusBadge status={task.status} />} />
        <Row k="workflow" v={task.workflow_name || "—"} />
        <Row k="step" v={task.step_name || "—"} />
        <Row k="agent" v={task.agent_name || "—"} />
        <Row k="author" v={task.author_name || task.author_id} />
        <Row k="comments" v={String(task.comment_count)} />
        <Row k="created" v={localTime(task.created_at)} />
        <Row k="updated" v={localTime(task.updated_at)} />
      </dl>
      {task.blocked_by.length > 0 && (
        <div className="blockers">
          <div className="blockers-title">blocked by</div>
          {task.blocked_by.map((b) => (
            <div key={b.id} className="blocker-row">
              <StatusBadge status={b.status} /> <span className="mono">{b.id}</span>
              <button className="btn-link" onClick={() => void unblock(b.id)}>
                unblock
              </button>
            </div>
          ))}
        </div>
      )}
      {task.blocks.length > 0 && (
        <div className="blockers">
          <div className="blockers-title">blocks</div>
          {task.blocks.map((b) => (
            <div key={b.id} className="blocker-row">
              <StatusBadge status={b.status} /> <span className="mono">{b.id}</span>
            </div>
          ))}
        </div>
      )}
    </Section>
  );
}

function Row({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="meta-row">
      <dt>{k}</dt>
      <dd>{v}</dd>
    </div>
  );
}

function WorkflowGraph({ workflow, currentStepId }: { workflow: Workflow; currentStepId: string }) {
  return (
    <Section title={`Workflow · ${workflow.name}`}>
      <div className="wf-graph">
        {workflow.steps.map((s) => {
          const current = s.id === currentStepId;
          const targets = [...s.next_steps, ...s.next_status];
          return (
            <div key={s.id} className={`wf-step ${current ? "wf-current" : ""}`}>
              <div className="wf-step-head">
                <span className="wf-step-name">{s.name}</span>
                {s.max_visits > 0 && <span className="wf-cap">≤{s.max_visits}</span>}
              </div>
              <div className="wf-step-agent">{s.agent_name || s.agent_id}</div>
              {targets.length > 0 && (
                <div className="wf-step-next">→ {targets.join(", ")}</div>
              )}
            </div>
          );
        })}
      </div>
      <div className="wf-isolation">isolation: {workflow.isolation || "none"}</div>
    </Section>
  );
}

function VisitCounters({ task, workflow }: { task: TaskView; workflow: Workflow }) {
  const visits = useMemo(() => {
    const meta = task.metadata as Record<string, unknown> | null;
    const raw = meta?.step_visits;
    if (!raw || typeof raw !== "object") return [];
    const map = raw as Record<string, number>;
    return workflow.steps
      .map((s) => ({ name: s.name, used: map[s.id] ?? 0, cap: s.max_visits }))
      .filter((v) => v.used > 0 || v.cap > 0);
  }, [task.metadata, workflow.steps]);

  if (visits.length === 0) return null;
  return (
    <Section title="Visits">
      <div className="visit-list">
        {visits.map((v) => (
          <div key={v.name} className="visit-row">
            <span className="visit-name">{v.name}</span>
            <span className="visit-count">
              {v.used}
              {v.cap > 0 ? `/${v.cap}` : ""}
            </span>
          </div>
        ))}
      </div>
    </Section>
  );
}

function WorktreeInfo({
  task,
  isolation,
  projectRoot,
}: {
  task: TaskView;
  isolation: string;
  projectRoot: string;
}) {
  // branch convention is exact (autosk/<task-id>, per autosk-core/worktree.rs).
  const branch = `autosk/${task.id}`;
  // The on-disk layout is ~/.autosk/worktrees/<basename>-<hash>/<task-id> where
  // <hash> is 8 hex of sha256(canonical project root) — not derivable in the
  // browser. Surface the real basename and present the rest as the documented
  // layout convention (labelled "layout"), not as resolved data.
  const base = projectRoot ? projectRoot.replace(/\/+$/, "").split("/").pop() || "" : "";
  const slug = base ? `${base}-<hash>` : "<proj>-<hash>";
  return (
    <Section title="Worktree">
      <dl className="meta-list">
        <Row k="isolation" v={isolation || "none"} />
        <Row k="branch" v={<span className="mono">{branch}</span>} />
        {isolation === "worktree" ? (
          <Row
            k="layout"
            v={<span className="mono dim">~/.autosk/worktrees/{slug}/{task.id}</span>}
          />
        ) : (
          <Row k="path" v={<span className="dim">runs in the project root</span>} />
        )}
      </dl>
    </Section>
  );
}

// ---- modals ---------------------------------------------------------------

function EditTaskModal({
  task,
  cwd,
  onClose,
  onSaved,
}: {
  task: TaskView;
  cwd: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { effects } = useStore();
  const [title, setTitle] = useState(task.title);
  const [description, setDescription] = useState(task.description);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    if (!title.trim()) return;
    setBusy(true);
    try {
      await ipc.taskSetTitleDescription(cwd, task.id, title, description);
      onSaved();
      onClose();
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title={`Edit ${task.id}`}
      onClose={onClose}
      footer={
        <button className="btn btn-primary" disabled={busy || !title.trim()} onClick={() => void save()}>
          Save
        </button>
      }
    >
      <label className="field">
        <span className="field-label">Title</span>
        <input className="input" value={title} onChange={(e) => setTitle(e.target.value)} />
      </label>
      <label className="field">
        <span className="field-label">Description</span>
        <textarea className="textarea" rows={8} value={description} onChange={(e) => setDescription(e.target.value)} />
      </label>
    </Modal>
  );
}

function MetadataModal({
  task,
  cwd,
  onClose,
  onSaved,
}: {
  task: TaskView;
  cwd: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [text, setText] = useState(JSON.stringify(task.metadata ?? {}, null, 2));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const save = async () => {
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(text || "{}");
    } catch {
      setErr("Invalid JSON.");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await ipc.taskSetMetadata(cwd, task.id, parsed);
      onSaved();
      onClose();
    } catch (e) {
      setErr(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title={`Metadata · ${task.id}`}
      onClose={onClose}
      footer={
        <button className="btn btn-primary" disabled={busy} onClick={() => void save()}>
          Replace metadata
        </button>
      }
    >
      <p className="hint">Wholesale-replaces the task's metadata JSON (mirrors lazy's "M" hotkey).</p>
      <textarea className="textarea mono" rows={14} value={text} onChange={(e) => setText(e.target.value)} />
      {err && <div className="form-error">{err}</div>}
    </Modal>
  );
}
