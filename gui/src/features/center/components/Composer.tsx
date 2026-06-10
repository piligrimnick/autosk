// Composer — the input pinned at the bottom of the center panel. Mode is
// resolved by `composerMode(state)`:
//   steer   → running/queued session selected → steer the agent (the abort /
//             cancel job controls live in the session header, not here)
//   comment → a task selected (any status) → add a comment; enroll / resume /
//             reopen moved to the Enroll button in the task header
//   none    → nothing renders (also covers a terminal, read-only session)

import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { activeTask, composerMode, selectedSessionJob } from "@/state/selectors";
import { ComposerInput } from "./ComposerInput";

export function Composer() {
  const { state } = useStore();
  switch (composerMode(state)) {
    case "steer":
      return <SteerComposer />;
    case "comment":
      return <CommentComposer />;
    default:
      return null;
  }
}

function useCwd() {
  const { state } = useStore();
  return state.activeProject ?? "";
}

// ---- running/queued session: steer the agent ------------------------------

function SteerComposer() {
  const { state, effects } = useStore();
  const cwd = useCwd();
  const job = selectedSessionJob(state)!;

  const send = async (text: string) => {
    try {
      await ipc.jobInput(cwd, job.job_id, text, "steer");
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    }
  };

  return (
    <div className="composer">
      <ComposerInput placeholder="Steer the agent…" sendTitle="Steer" onSubmit={send} />
    </div>
  );
}

// ---- task (any status): add a comment -------------------------------------

function CommentComposer() {
  const { state, effects } = useStore();
  const cwd = useCwd();
  const task = activeTask(state)!;

  const add = async (text: string) => {
    try {
      await ipc.commentAdd(cwd, task.id, text);
      await effects.refreshTask(task.id);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    }
  };

  return (
    <div className="composer">
      <ComposerInput placeholder="Add a comment ..." sendTitle="Add comment" onSubmit={add} />
    </div>
  );
}
