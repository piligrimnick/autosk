// CenterPanel — the center region (redesign plan §8.2/§8.3): the project +
// entity-id header, the polymorphic body (task | session | workflow | empty),
// and the unified composer pinned at the bottom. Phase 3 wires the session
// view; the task view (Phase 4) and workflow view (Phase 5) are placeholders.

import { useStore } from "@/state/store";
import { CenterHeader } from "./CenterHeader";
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
      <CenterHeader />
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
