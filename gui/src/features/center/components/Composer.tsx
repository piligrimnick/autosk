// Composer — the unified, entity-aware input at the bottom of the center panel
// (redesign plan §8.4). Mode is resolved by `composerMode(state)`:
//   steer    → session selected, job running/queued → steer/follow-up/abort/cancel
//   readonly → session selected, job terminal → muted strip, no input
//   (task modes: new/human/enrolled/terminal land in Phase 4)
//   none     → nothing renders

import { useMemo, useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeSlice, activeTask, composerMode, selectedSessionJob } from "@/state/selectors";

export function Composer() {
  const { state } = useStore();
  const mode = composerMode(state);
  switch (mode) {
    case "steer":
      return <SteerComposer />;
    case "readonly":
      return <ReadonlyComposer />;
    case "new":
      return <EnrollComposer />;
    case "human":
      return <HumanComposer />;
    case "enrolled":
      return <CommentComposer hint="Task is enrolled and waiting. Leave a note for the next step." />;
    case "terminal":
      return <TerminalComposer />;
    default:
      return null;
  }
}

function useCwd() {
  const { state } = useStore();
  return state.activeProject ?? "";
}

// ---- running/queued session: steer / follow_up / abort / cancel -----------

function SteerComposer() {
  const { state, effects } = useStore();
  const cwd = useCwd();
  const job = selectedSessionJob(state)!;
  const queued = job.status === "queued";
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);

  const send = async (behavior: "steer" | "follow_up") => {
    if (!text.trim()) return;
    setBusy(true);
    try {
      await ipc.jobInput(cwd, job.job_id, text, behavior);
      setText("");
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  const abort = async () => {
    if (!confirm("Abort the current turn?")) return;
    setBusy(true);
    try {
      await ipc.jobAbort(cwd, job.job_id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  // Cancel (lazy `K`). Distinct from abort: job.cancel stops BOTH a running run
  // and a QUEUED run and is idempotent on terminal; job.abort needs a live
  // runner and CONFLICTs on a queued/terminal run.
  const cancel = async () => {
    const verb = queued ? "Cancel this queued job" : "Cancel (kill) the running job";
    if (!confirm(`${verb} ${job.job_id.slice(0, 8)}?`)) return;
    setBusy(true);
    try {
      await ipc.jobCancel(cwd, job.job_id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  const onKey = (e: React.KeyboardEvent) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      void send("steer");
    }
  };

  return (
    <div className="composer composer-running">
      <div className="composer-state">
        <span className={`run-indicator ${job.streaming ? "streaming" : ""}`}>●</span>{" "}
        {queued ? "queued" : "running"} · job {job.job_id.slice(0, 8)}
        {job.streaming ? " · streaming" : ""}
      </div>
      <textarea
        className="composer-input"
        placeholder="Steer the agent (Cmd/Ctrl+Enter), or send a follow-up…"
        value={text}
        disabled={busy}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKey}
      />
      <div className="composer-actions">
        <button className="btn btn-primary" disabled={busy || !text.trim()} onClick={() => void send("steer")}>
          Steer ⌘↵
        </button>
        <button className="btn" disabled={busy || !text.trim()} onClick={() => void send("follow_up")}>
          Follow-up
        </button>
        <button
          className="btn btn-danger"
          disabled={busy || queued}
          title={queued ? "Abort needs a live runner; use Cancel job for a queued job" : "Interrupt the current turn"}
          onClick={() => void abort()}
        >
          Abort
        </button>
        <button
          className="btn btn-danger"
          disabled={busy}
          title="Cancel (kill) this job — works for queued and running"
          onClick={() => void cancel()}
        >
          Cancel job
        </button>
      </div>
    </div>
  );
}

// ---- terminal session: read-only ------------------------------------------

function ReadonlyComposer() {
  const { state } = useStore();
  const job = selectedSessionJob(state);
  return (
    <div className="composer composer-readonly">
      <div className="composer-state">session is {job?.status ?? "—"} · read-only</div>
    </div>
  );
}

// ---- task: human-parked — comment + resume --------------------------------

function HumanComposer() {
  const { state, effects } = useStore();
  const cwd = useCwd();
  const task = activeTask(state)!;
  const slice = activeSlice(state);
  const [text, setText] = useState("");
  const [toStep, setToStep] = useState("");
  const [busy, setBusy] = useState(false);

  const stepNames = useMemo(() => {
    const wf = slice.workflows.find((w) => w.id === task.workflow_id);
    return wf ? wf.steps.map((s) => s.name) : [];
  }, [slice.workflows, task.workflow_id]);

  const addComment = async () => {
    if (!text.trim()) return;
    setBusy(true);
    try {
      await ipc.commentAdd(cwd, task.id, text);
      setText("");
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  const resume = async () => {
    setBusy(true);
    try {
      if (text.trim()) {
        await ipc.commentAdd(cwd, task.id, text);
        setText("");
      }
      await ipc.taskResume(cwd, task.id, toStep);
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="composer composer-human">
      <div className="composer-state">⏸ waiting for human</div>
      <textarea
        className="composer-input"
        placeholder="Add a comment for the agent, then resume…"
        value={text}
        disabled={busy}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="composer-actions">
        <button className="btn" disabled={busy || !text.trim()} onClick={() => void addComment()}>
          Add comment
        </button>
        <label className="composer-inline">
          resume to
          <select className="select" value={toStep} onChange={(e) => setToStep(e.target.value)} disabled={busy}>
            <option value="">(current step)</option>
            {stepNames.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
        <button className="btn btn-primary" disabled={busy} onClick={() => void resume()}>
          Resume
        </button>
      </div>
    </div>
  );
}

// ---- task: new — enroll ---------------------------------------------------

function EnrollComposer() {
  const { state, effects } = useStore();
  const cwd = useCwd();
  const task = activeTask(state)!;
  const slice = activeSlice(state);
  const [kind, setKind] = useState<"workflow" | "agent">("workflow");
  const [workflow, setWorkflow] = useState("");
  const [agent, setAgent] = useState("");
  const [step, setStep] = useState("");
  const [busy, setBusy] = useState(false);

  const selectedWf = slice.workflows.find((w) => w.name === workflow);
  const steps = selectedWf ? selectedWf.steps.map((s) => s.name) : [];

  const enroll = async () => {
    setBusy(true);
    try {
      if (kind === "workflow" && !workflow) throw new Error("Pick a workflow.");
      if (kind === "agent" && !agent) throw new Error("Pick an agent.");
      const args =
        kind === "workflow" ? { workflow, step: step || undefined } : { agent, step: step || undefined };
      await ipc.taskEnroll(cwd, task.id, args);
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="composer composer-enroll">
      <div className="composer-state">🆕 new — enroll to start work</div>
      <div className="composer-actions composer-wrap">
        <div className="seg">
          <button className={`seg-btn ${kind === "workflow" ? "seg-active" : ""}`} onClick={() => setKind("workflow")}>
            Workflow
          </button>
          <button className={`seg-btn ${kind === "agent" ? "seg-active" : ""}`} onClick={() => setKind("agent")}>
            Single agent
          </button>
        </div>
        {kind === "workflow" ? (
          <select className="select" value={workflow} onChange={(e) => setWorkflow(e.target.value)} disabled={busy}>
            <option value="">Select workflow…</option>
            {slice.workflows.map((w) => (
              <option key={w.id} value={w.name}>
                {w.name}
              </option>
            ))}
          </select>
        ) : (
          <select className="select" value={agent} onChange={(e) => setAgent(e.target.value)} disabled={busy}>
            <option value="">Select agent…</option>
            {slice.agents.map((a) => (
              <option key={a.id} value={a.name}>
                {a.name}
              </option>
            ))}
          </select>
        )}
        {kind === "workflow" && steps.length > 0 && (
          <select className="select" value={step} onChange={(e) => setStep(e.target.value)} disabled={busy}>
            <option value="">(first step)</option>
            {steps.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        )}
        <button className="btn btn-primary" disabled={busy} onClick={() => void enroll()}>
          Enroll
        </button>
      </div>
    </div>
  );
}

// ---- task: work (no running job) — comment ---------------------------------

function CommentComposer({ hint }: { hint: string }) {
  const { state, effects } = useStore();
  const cwd = useCwd();
  const task = activeTask(state)!;
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);

  const add = async () => {
    if (!text.trim()) return;
    setBusy(true);
    try {
      await ipc.commentAdd(cwd, task.id, text);
      setText("");
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="composer composer-comment">
      <div className="composer-state">{hint}</div>
      <textarea
        className="composer-input"
        placeholder="Add a comment…"
        value={text}
        disabled={busy}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="composer-actions">
        <button className="btn btn-primary" disabled={busy || !text.trim()} onClick={() => void add()}>
          Add comment
        </button>
      </div>
    </div>
  );
}

// ---- task: terminal — reopen ----------------------------------------------

function TerminalComposer() {
  const { state, effects } = useStore();
  const cwd = useCwd();
  const task = activeTask(state)!;
  const [busy, setBusy] = useState(false);

  const reopen = async () => {
    setBusy(true);
    try {
      await ipc.taskReopen(cwd, task.id);
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="composer composer-terminal">
      <div className="composer-state">task is {task.status}</div>
      <div className="composer-actions">
        <button className="btn btn-primary" disabled={busy} onClick={() => void reopen()}>
          Reopen
        </button>
      </div>
    </div>
  );
}
