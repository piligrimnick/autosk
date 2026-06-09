// TaskRowMenu — per-task write verbs reachable from the Tasks panel row kebab
// (redesign plan §8.5). Status flips (done/cancel/reopen) fire directly; edit /
// metadata / block open small modals (co-located here). Enroll/resume/comment
// live in the center composer, so they are not duplicated here.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import type { TaskView } from "@/types";
import { Modal } from "@/components/Modal";
import { Menu, MenuDivider, MenuItem, MenuLabel } from "@/features/shared/Menu";

const TERMINAL = new Set(["done", "cancel"]);

export function TaskRowMenu({ task, anchor, onClose }: { task: TaskView; anchor: DOMRect | null; onClose: () => void }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const [modal, setModal] = useState<"edit" | "metadata" | "block" | null>(null);
  const open = anchor !== null;

  const run = async (fn: () => Promise<unknown>) => {
    onClose();
    try {
      await fn();
      await effects.refreshTasks();
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    }
  };

  const terminal = TERMINAL.has(task.status);

  return (
    <>
      <Menu open={open} anchor={anchor} onClose={onClose}>
        <MenuLabel>{task.id}</MenuLabel>
        {task.status !== "done" && (
          <MenuItem onClick={() => void run(() => ipc.taskDone(cwd, task.id))}>Mark done</MenuItem>
        )}
        {task.status !== "cancel" && (
          <MenuItem onClick={() => void run(() => ipc.taskCancel(cwd, task.id))}>Cancel</MenuItem>
        )}
        {terminal && <MenuItem onClick={() => void run(() => ipc.taskReopen(cwd, task.id))}>Reopen</MenuItem>}
        <MenuDivider />
        <MenuItem onClick={() => { onClose(); setModal("edit"); }}>Edit…</MenuItem>
        <MenuItem onClick={() => { onClose(); setModal("metadata"); }}>Metadata…</MenuItem>
        <MenuItem onClick={() => { onClose(); setModal("block"); }}>Add blocker…</MenuItem>
        {task.blocked_by.length > 0 && <MenuDivider />}
        {task.blocked_by.map((b) => (
          <MenuItem key={b.id} onClick={() => void run(() => ipc.taskUnblock(cwd, task.id, [b.id]))}>
            Unblock {b.id}
          </MenuItem>
        ))}
      </Menu>

      {modal === "edit" && <EditTaskModal task={task} cwd={cwd} onClose={() => setModal(null)} />}
      {modal === "metadata" && <MetadataModal task={task} cwd={cwd} onClose={() => setModal(null)} />}
      {modal === "block" && <BlockModal task={task} cwd={cwd} onClose={() => setModal(null)} />}
    </>
  );
}

function useRefresh(taskId: string) {
  const { effects } = useStore();
  return async () => {
    await effects.refreshTasks();
    await effects.refreshTask(taskId);
  };
}

function EditTaskModal({ task, cwd, onClose }: { task: TaskView; cwd: string; onClose: () => void }) {
  const { effects } = useStore();
  const refresh = useRefresh(task.id);
  const [title, setTitle] = useState(task.title);
  const [description, setDescription] = useState(task.description);
  const [priority, setPriority] = useState(task.priority);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    if (!title.trim()) return;
    setBusy(true);
    try {
      await ipc.taskUpdate(cwd, task.id, { title, description, priority });
      await refresh();
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
      <label className="field">
        <span className="field-label">Priority</span>
        <select className="select" value={priority} onChange={(e) => setPriority(Number(e.target.value))}>
          {[0, 1, 2, 3].map((p) => (
            <option key={p} value={p}>
              P{p}
            </option>
          ))}
        </select>
      </label>
    </Modal>
  );
}

function MetadataModal({ task, cwd, onClose }: { task: TaskView; cwd: string; onClose: () => void }) {
  const refresh = useRefresh(task.id);
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
      await refresh();
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

function BlockModal({ task, cwd, onClose }: { task: TaskView; cwd: string; onClose: () => void }) {
  const { effects } = useStore();
  const refresh = useRefresh(task.id);
  const [blocker, setBlocker] = useState("");
  const [busy, setBusy] = useState(false);

  const add = async () => {
    const id = blocker.trim();
    if (!id) return;
    setBusy(true);
    try {
      await ipc.taskBlock(cwd, task.id, [id]);
      await refresh();
      onClose();
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title={`Add blocker · ${task.id}`}
      onClose={onClose}
      footer={
        <button className="btn btn-primary" disabled={busy || !blocker.trim()} onClick={() => void add()}>
          Block
        </button>
      }
    >
      <label className="field">
        <span className="field-label">Blocker task id</span>
        <input className="input" autoFocus value={blocker} placeholder="ask-…" onChange={(e) => setBlocker(e.target.value)} />
      </label>
    </Modal>
  );
}
