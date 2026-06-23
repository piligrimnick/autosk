// EnrollModal — the task "Enroll" picker, opened from the Enroll button in the
// task header. Mirrors lazy-mode's two-pane picker: a workflow list on the left
// and the selected workflow's step list on the right (no dropdowns). Single
// agents are intentionally excluded (synthetic single:<agent> workflows).
//
// The cursor is seeded to the task's current workflow + step (the natural
// "continue where it left off" default), but any workflow and step can be
// chosen. Confirm always calls task.enroll({ workflow, step }) — enroll honors
// the picked step and is accepted from new / cancel / human. It is rejected on
// `work` (a live run, abort first) and `done` (terminal — reopen it), which is
// why the Enroll button is hidden while the task is in `work`.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeSlice } from "@/state/selectors";
import { Modal } from "@/components/Modal";
import type { TaskView, WorkflowInfo } from "@/types";

/** The Enroll button + its modal. Rendered in the task header; hidden while the
 *  task is in `work` (a live run), where enroll would conflict. */
export function EnrollButton({ task }: { task: TaskView }) {
  const [open, setOpen] = useState(false);
  if (task.status === "work") return null;
  return (
    <>
      <button className="btn btn-sm btn-primary task-view-enroll-btn" onClick={() => setOpen(true)}>
        Enroll
      </button>
      {open && createPortal(<EnrollModal task={task} onClose={() => setOpen(false)} />, document.body)}
    </>
  );
}

function EnrollModal({ task, onClose }: { task: TaskView; onClose: () => void }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const workflows = useMemo<WorkflowInfo[]>(() => activeSlice(state).workflows, [state]);

  // Seed the cursor on the task's current workflow (else the first one).
  const initialWf = useMemo(() => {
    const byName = workflows.findIndex((w) => w.name === task.workflow);
    return byName >= 0 ? byName : 0;
  }, [workflows, task.workflow]);

  const [wfIdx, setWfIdx] = useState(initialWf);
  const [stepIdx, setStepIdx] = useState(0);
  const [focus, setFocus] = useState<"wf" | "step">("wf");
  const [busy, setBusy] = useState(false);
  const boxRef = useRef<HTMLDivElement>(null);

  const wf = workflows[wfIdx];
  const steps = wf?.steps ?? [];

  // Seed the step cursor to the current step whenever the workflow under the
  // cursor is the task's current workflow; otherwise land on the first step.
  useEffect(() => {
    if (wf && wf.name === task.workflow && task.step) {
      const i = steps.findIndex((s) => s.name === task.step);
      setStepIdx(i >= 0 ? i : 0);
    } else {
      setStepIdx(0);
    }
  }, [wfIdx]);

  useEffect(() => {
    boxRef.current?.focus();
  }, []);

  const confirm = useCallback(async () => {
    if (!wf || busy) return;
    const stepName = steps[stepIdx]?.name ?? "";
    setBusy(true);
    try {
      // Enroll honors the picked step (omitted ⇒ the workflow's first step).
      await ipc.taskEnroll(cwd, task.id, stepName ? { workflow: wf.name, step: stepName } : { workflow: wf.name });
      await effects.refreshTask(task.id);
      await effects.refreshTasks();
      onClose();
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  }, [wf, busy, steps, stepIdx, cwd, task.id, effects, onClose]);

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter") {
      e.preventDefault();
      void confirm();
      return;
    }
    if (e.key === "ArrowLeft") {
      setFocus("wf");
      return;
    }
    if (e.key === "ArrowRight") {
      if (steps.length > 0) setFocus("step");
      return;
    }
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const dir = e.key === "ArrowDown" ? 1 : -1;
      if (focus === "wf") {
        setWfIdx((i) => clamp(i + dir, workflows.length));
      } else {
        setStepIdx((i) => clamp(i + dir, steps.length));
      }
    }
  };

  return (
    <Modal
      title={`Enroll · ${task.id}`}
      onClose={onClose}
      footer={
        <button className="btn btn-primary" disabled={busy || !wf} onClick={() => void confirm()}>
          Enroll
        </button>
      }
    >
      {workflows.length === 0 ? (
        <p className="hint">No workflows installed in this project.</p>
      ) : (
        <div className="enroll-panes" ref={boxRef} tabIndex={0} onKeyDown={onKeyDown}>
          <div className={`enroll-pane ${focus === "wf" ? "is-focused" : ""}`}>
            <div className="enroll-pane-head">Workflow</div>
            <div className="enroll-list">
              {workflows.map((w, i) => (
                <button
                  key={w.name}
                  className={`enroll-item ${i === wfIdx ? "sel" : ""}`}
                  onClick={() => {
                    setWfIdx(i);
                    setFocus("wf");
                  }}
                  onDoubleClick={() => void confirm()}
                >
                  {w.name}
                </button>
              ))}
            </div>
          </div>
          <div className={`enroll-pane ${focus === "step" ? "is-focused" : ""}`}>
            <div className="enroll-pane-head">Step</div>
            <div className="enroll-list">
              {steps.map((s, i) => (
                <button
                  key={s.name}
                  className={`enroll-item ${i === stepIdx ? "sel" : ""}`}
                  onClick={() => {
                    setStepIdx(i);
                    setFocus("step");
                  }}
                  onDoubleClick={() => void confirm()}
                >
                  {s.name}
                </button>
              ))}
            </div>
          </div>
        </div>
      )}
    </Modal>
  );
}

function clamp(i: number, len: number): number {
  if (len <= 0) return 0;
  return Math.max(0, Math.min(i, len - 1));
}
