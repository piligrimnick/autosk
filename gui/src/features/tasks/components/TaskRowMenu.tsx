// useTaskRowMenu — the per-task write-verb menu for a Tasks-panel row, rendered
// as a NATIVE OS context menu (Tauri `@tauri-apps/api/menu`) popped at the
// cursor on right-click, mirroring CodexMonitor's sidebar menus. Status flips
// (done/cancel/reopen) and unblock fire IPC directly; Edit / Metadata / Add
// blocker open the co-located React modals (enroll/resume/comment live in the
// center composer, so they are not duplicated here). Returns the row's
// `onContextMenu` handler plus the modal tree to render — the modals are
// portaled to <body>, so their clicks never bubble to the row's select handler.

import { useCallback, useState, type MouseEvent, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Menu, MenuItem, PredefinedMenuItem } from "@tauri-apps/api/menu";
import { LogicalPosition } from "@tauri-apps/api/dpi";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import type { TaskView } from "@/types";
import { Modal } from "@/components/Modal";

const TERMINAL = new Set(["done", "cancel"]);

export function useTaskRowMenu(task: TaskView): {
  openMenu: (e: MouseEvent) => Promise<void>;
  modals: ReactNode;
} {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const [modal, setModal] = useState<"edit" | "metadata" | "block" | null>(null);

  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      try {
        await fn();
        await effects.refreshTasks();
        await effects.refreshTask(task.id);
      } catch (err) {
        effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
      }
    },
    [effects, task.id],
  );

  const openMenu = useCallback(
    async (e: MouseEvent) => {
      // Suppress the webview's native context menu and pop our own at the
      // cursor. Capture the coordinates before the first await — the menu is
      // built asynchronously (each item crosses the Tauri bridge).
      e.preventDefault();
      e.stopPropagation();
      const { clientX, clientY } = e;
      try {
        const items: (MenuItem | PredefinedMenuItem)[] = [];
        items.push(await MenuItem.new({ text: task.id, enabled: false }));
        items.push(await PredefinedMenuItem.new({ item: "Separator" }));
        if (task.status !== "done") {
          items.push(await MenuItem.new({ text: "Mark done", action: () => void run(() => ipc.taskDone(cwd, task.id)) }));
        }
        if (task.status !== "cancel") {
          items.push(await MenuItem.new({ text: "Cancel", action: () => void run(() => ipc.taskCancel(cwd, task.id)) }));
        }
        if (TERMINAL.has(task.status)) {
          items.push(await MenuItem.new({ text: "Reopen", action: () => void run(() => ipc.taskReopen(cwd, task.id)) }));
        }
        items.push(await PredefinedMenuItem.new({ item: "Separator" }));
        items.push(await MenuItem.new({ text: "Edit…", action: () => setModal("edit") }));
        items.push(await MenuItem.new({ text: "Metadata…", action: () => setModal("metadata") }));
        items.push(await MenuItem.new({ text: "Add blocker…", action: () => setModal("block") }));
        if (task.blocked_by.length > 0) {
          items.push(await PredefinedMenuItem.new({ item: "Separator" }));
          for (const b of task.blocked_by) {
            items.push(await MenuItem.new({ text: `Unblock ${b.id}`, action: () => void run(() => ipc.taskUnblock(cwd, task.id, [b.id])) }));
          }
        }
        const menu = await Menu.new({ items });
        await menu.popup(new LogicalPosition(clientX, clientY), getCurrentWindow());
      } catch (err) {
        effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
      }
    },
    [cwd, effects, run, task.blocked_by, task.id, task.status],
  );

  const modals = modal
    ? createPortal(
        <>
          {modal === "edit" && <EditTaskModal task={task} cwd={cwd} onClose={() => setModal(null)} />}
          {modal === "metadata" && <MetadataModal task={task} cwd={cwd} onClose={() => setModal(null)} />}
          {modal === "block" && <BlockModal task={task} cwd={cwd} onClose={() => setModal(null)} />}
        </>,
        document.body,
      )
    : null;

  return { openMenu, modals };
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
