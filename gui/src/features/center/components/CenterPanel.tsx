// CenterPanel — the center region (redesign plan §8.2/§8.3): the polymorphic
// body (task | session | workflow | empty) and the unified composer pinned at
// the bottom. The project switcher lives in the titlebar (status bar) and the
// selected entity id is surfaced by each view, so the center panel has no
// header row of its own.

import { useStore } from "@/state/store";
import { Composer } from "./Composer";
import { SessionView } from "../views/SessionView";
import { TaskView } from "../views/TaskView";
import { WorkflowView } from "../views/WorkflowView";
import { EmptyView } from "../views/EmptyView";

export function CenterPanel() {
  const { state } = useStore();
  const sel = state.selection;

  return (
    <main className="panel panel-center">
      <div className="panel-body center-body">
        {sel.kind === "session" ? (
          <SessionView />
        ) : sel.kind === "task" ? (
          <TaskView />
        ) : sel.kind === "workflow" ? (
          <WorkflowView />
        ) : (
          <EmptyView />
        )}
      </div>
      <Composer />
    </main>
  );
}
