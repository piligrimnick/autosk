// SessionStatusDot — animated status indicator for a session/job row
// (redesign plan §8.1). CodexMonitor-derived pulse for active runs.

import type { Job } from "@/types";

function dotKind(status: string): string {
  switch (status) {
    case "running":
      return "running";
    case "queued":
      return "queued";
    case "done":
      return "done";
    case "failed":
      return "failed";
    case "cancelled":
    case "cancel":
      return "cancelled";
    default:
      return "other";
  }
}

export function SessionStatusDot({ job }: { job: Job }) {
  const kind = dotKind(job.status);
  const streaming = job.status === "running" && job.streaming;
  return (
    <span
      className={`session-dot session-dot-${kind}${streaming ? " is-streaming" : ""}`}
      title={streaming ? "streaming" : job.status}
      aria-hidden
    />
  );
}
