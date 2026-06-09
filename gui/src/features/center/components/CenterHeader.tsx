// CenterHeader — the center panel header (redesign plan §8.2): the project
// switcher on the left and the selected entity id on the right.

import { useStore } from "@/state/store";
import { selectedSessionJob } from "@/state/selectors";
import type { AppState } from "@/state/types";
import { ProjectSwitcher } from "@/features/projects/components/ProjectSwitcher";

export function CenterHeader() {
  const { state } = useStore();
  const objId = entityId(state);

  return (
    <div className="panel-header center-header">
      <ProjectSwitcher />
      {objId && <span className="center-objid">{objId}</span>}
    </div>
  );
}

function entityId(state: AppState): string {
  const sel = state.selection;
  switch (sel.kind) {
    case "task":
      return sel.taskId;
    case "session": {
      const job = selectedSessionJob(state);
      return (job ? job.job_id : sel.jobId).slice(0, 8);
    }
    case "workflow":
      return sel.name;
    default:
      return "";
  }
}
