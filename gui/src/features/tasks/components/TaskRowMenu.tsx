// useTaskRowMenu — the per-task write-verb menu for a Tasks-panel row, rendered
// as a NATIVE OS context menu (Tauri `@tauri-apps/api/menu`) popped at the
// cursor on right-click, mirroring CodexMonitor's sidebar menus. The menu is
// built from a plain entry list so that when the native menu plugin is
// unavailable (iOS/Android — "plugin menu not found") the SAME entries render
// as an in-app popover at the same position instead. Status flips
// (done/cancel/reopen) and unblock fire IPC directly; Edit / Add blocker open
// the co-located React modals (enroll/resume/comment live in the center
// composer, so they are not duplicated here). Returns the row's
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

/** One menu entry — the shared shape behind both the native and in-app menus. */
type MenuEntry =
  | { kind: "item"; text: string; enabled?: boolean; action?: () => void }
  | { kind: "separator" };

export function useTaskRowMenu(task: TaskView): {
  openMenu: (e: MouseEvent) => Promise<void>;
  modals: ReactNode;
} {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const [modal, setModal] = useState<"edit" | "block" | null>(null);

  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      try {
        await fn();
        await effects.refreshTasks();
        // refreshTasks no longer pulls sessions; a status flip (e.g. cancel may
        // abort a running session) can move them, so refresh the panel too.
        await effects.refreshSessions();
        await effects.refreshTask(task.id);
      } catch (err) {
        effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
      }
    },
    [effects, task.id],
  );

  // done/cancel: the daemon reaps the task's worktree (branch preserved). If it
  // is dirty and `force` was not set, the verb fails with EnvironmentDirty; we
  // then confirm a forced retry (which discards the uncommitted changes).
  const runTerminal = useCallback(
    (verb: "done" | "cancel") => {
      const call = (force: boolean) =>
        verb === "done" ? ipc.taskDone(cwd, task.id, force) : ipc.taskCancel(cwd, task.id, force);
      void run(async () => {
        try {
          await call(false);
        } catch (err) {
          if (err instanceof ipc.DaemonError && err.code === ipc.ErrorCode.EnvironmentDirty) {
            const label = verb === "done" ? "Mark done" : "Cancel";
            if (!confirm(`${task.id}: isolation environment has uncommitted changes.\n${label} with force (discards them)?`)) {
              return; // declined — leave the task as-is
            }
            await call(true);
            return;
          }
          throw err; // surface every other error via run()'s notice
        }
      });
    },
    [cwd, run, task.id],
  );

  const [popover, setPopover] = useState<{ x: number; y: number; entries: MenuEntry[] } | null>(null);

  const buildEntries = useCallback((): MenuEntry[] => {
    const entries: MenuEntry[] = [];
    entries.push({ kind: "item", text: task.id, enabled: false });
    entries.push({ kind: "separator" });
    if (task.status !== "done") {
      entries.push({ kind: "item", text: "Mark done", action: () => runTerminal("done") });
    }
    if (task.status !== "cancel") {
      entries.push({ kind: "item", text: "Cancel", action: () => runTerminal("cancel") });
    }
    if (TERMINAL.has(task.status)) {
      entries.push({ kind: "item", text: "Reopen", action: () => void run(() => ipc.taskReopen(cwd, task.id)) });
    }
    entries.push({ kind: "separator" });
    entries.push({ kind: "item", text: "Edit…", action: () => setModal("edit") });
    entries.push({ kind: "item", text: "Add blocker…", action: () => setModal("block") });
    if (task.blocked_by.length > 0) {
      entries.push({ kind: "separator" });
      for (const b of task.blocked_by) {
        entries.push({ kind: "item", text: `Unblock ${b.id}`, action: () => void run(() => ipc.taskUnblock(cwd, task.id, b.id)) });
      }
    }
    return entries;
  }, [cwd, run, runTerminal, task.blocked_by, task.id, task.status]);

  const openMenu = useCallback(
    async (e: MouseEvent) => {
      // Suppress the webview's native context menu and pop our own at the
      // cursor. Capture the coordinates before the first await — the menu is
      // built asynchronously (each item crosses the Tauri bridge).
      e.preventDefault();
      e.stopPropagation();
      const { clientX, clientY } = e;
      const entries = buildEntries();
      try {
        const items: (MenuItem | PredefinedMenuItem)[] = [];
        for (const en of entries) {
          items.push(
            en.kind === "separator"
              ? await PredefinedMenuItem.new({ item: "Separator" })
              : await MenuItem.new({ text: en.text, enabled: en.enabled !== false, action: en.action }),
          );
        }
        const menu = await Menu.new({ items });
        await menu.popup(new LogicalPosition(clientX, clientY), getCurrentWindow());
      } catch {
        // No native menu plugin (iOS/Android) or popup failure — render the
        // same entries as an in-app popover at the same position.
        setPopover({ x: clientX, y: clientY, entries });
      }
    },
    [buildEntries],
  );

  const modals =
    modal || popover
      ? createPortal(
          <>
            {popover && <FallbackMenu {...popover} onClose={() => setPopover(null)} />}
            {modal === "edit" && <EditTaskModal task={task} cwd={cwd} onClose={() => setModal(null)} />}
            {modal === "block" && <BlockModal task={task} cwd={cwd} onClose={() => setModal(null)} />}
          </>,
          document.body,
        )
      : null;

  return { openMenu, modals };
}

// FallbackMenu — the in-app stand-in for the native context menu on platforms
// without the menu plugin (mobile). Reuses the shared .menu* dropdown styles,
// fixed-positioned at the tap point (clamped to the viewport) and dismissed by
// tapping the backdrop. The leading disabled entry (the task id) renders as a
// .menu-label header.
function FallbackMenu({ x, y, entries, onClose }: { x: number; y: number; entries: MenuEntry[]; onClose: () => void }) {
  const height = entries.reduce((h, en) => h + (en.kind === "separator" ? 9 : 30), 8);
  const left = Math.max(8, Math.min(x, window.innerWidth - 200));
  const top = Math.max(8, Math.min(y, window.innerHeight - height - 8));
  return (
    <>
      <div
        className="menu-backdrop"
        onMouseDown={(e) => {
          e.stopPropagation();
          onClose();
        }}
        onContextMenu={(e) => {
          e.preventDefault();
          onClose();
        }}
      />
      <div className="menu" role="menu" style={{ position: "fixed", left, top, zIndex: 1001 }} onMouseDown={(e) => e.stopPropagation()}>
        {entries.map((en, i) =>
          en.kind === "separator" ? (
            <div key={i} className="menu-divider" />
          ) : en.enabled === false ? (
            <div key={i} className="menu-label">
              {en.text}
            </div>
          ) : (
            <button
              key={i}
              type="button"
              role="menuitem"
              className="menu-item"
              onClick={() => {
                onClose();
                en.action?.();
              }}
            >
              {en.text}
            </button>
          ),
        )}
      </div>
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
  const [busy, setBusy] = useState(false);

  const save = async () => {
    if (!title.trim()) return;
    setBusy(true);
    try {
      await ipc.taskUpdate(cwd, task.id, { title, description });
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
      await ipc.taskBlock(cwd, task.id, id);
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
