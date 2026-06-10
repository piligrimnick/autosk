// EnrollModal — the task "Enroll" picker, opened from the Enroll button in the
// task header. Mirrors lazy-mode's two-pane picker: a workflow list on the left
// and the selected workflow's step list on the right (no dropdowns). Single
// agents are intentionally excluded (synthetic single:<agent> workflows).
//
// The cursor is seeded to the task's current workflow + step, but any workflow
// and step can be chosen. The confirm verb is routed by task status:
//   - human + same workflow → task.resume(to_step)   (continue the run)
//   - otherwise (new / done / cancel, or human switching workflow) → task.enroll
// (enroll accepts new/human/done/cancel directly; it is rejected only on `work`,
// which is why the Enroll button is hidden while the task is enrolled.)

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeSlice } from "@/state/selectors";
import { Modal } from "@/components/Modal";
import type { TaskView, Workflow } from "@/types";

/** The Enroll button + its modal. Rendered in the task header; hidden while the
 *  task is enrolled (status `work`), where enroll would conflict. */
export function EnrollButton({ task }: { task: TaskView }) {
  const [open, setOpen] = useState(false);
  if (task.status === "work") return null;
  const label = task.status === "human" ? "Resume" : "Enroll";
  return (
    <>
      <button className="btn btn-sm btn-primary task-view-enroll-btn" onClick={() => setOpen(true)}>
        {label}
      </button>
      {open && createPortal(<EnrollModal task={task} onClose={() => setOpen(false)} />, document.body)}
    </>
  );
}

function EnrollModal({ task, onClose }: { task: TaskView; onClose: () => void }) {
  const { state, effects } = useStore();
  const cwd = state.activeProject ?? "";
  const workflows = useMemo<Workflow[]>(
    () => activeSlice(state).workflows.filter((w) => !w.is_synthetic),
    [state],
  );

  // Seed the cursor on the task's current workflow + step (else the first one).
  const initialWf = useMemo(() => {
    const byId = workflows.findIndex((w) => w.id === task.workflow_id);
    if (byId >= 0) return byId;
    const byName = workflows.findIndex((w) => w.name === task.workflow_name);
    return byName >= 0 ? byName : 0;
  }, [workflows, task.workflow_id, task.workflow_name]);

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
    if (wf && wf.name === task.workflow_name && task.step_name) {
      const i = steps.findIndex((s) => s.name === task.step_name);
      setStepIdx(i >= 0 ? i : 0);
    } else {
      setStepIdx(0);
    }
  }, [wfIdx]);

  useEffect(() => {
    boxRef.current?.focus();
  }, []);

  const sameWf = !!task.workflow_name && wf?.name === task.workflow_name;
  const isResume = task.status === "human" && sameWf;
  const confirmLabel = isResume ? "Resume" : "Enroll";

  const confirm = useCallback(async () => {
    if (!wf || busy) return;
    const stepName = steps[stepIdx]?.name ?? "";
    setBusy(true);
    try {
      if (isResume) {
        await ipc.taskResume(cwd, task.id, stepName === task.step_name ? "" : stepName);
      } else {
        await ipc.taskEnroll(cwd, task.id, { workflow: wf.name, step: stepName || undefined });
      }
      await effects.refreshTask(task.id);
      await effects.refreshTasks();
      onClose();
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  }, [wf, busy, steps, stepIdx, isResume, cwd, task.id, task.step_name, effects, onClose]);

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
          {confirmLabel}
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
                  key={w.id}
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
                  key={s.id}
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
