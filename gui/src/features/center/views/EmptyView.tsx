// EmptyView — the center body when nothing is selected (redesign plan §8.3).
// Reserved for future task-less interactive sessions (decision #1).

import { EmptyState } from "@/components/common";

export function EmptyView() {
  return (
    <div className="center-empty">
      <EmptyState title="Nothing selected" hint="Pick a session, task, or workflow to view it here." />
    </div>
  );
}
