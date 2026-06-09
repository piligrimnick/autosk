// Small shared presentational components + helpers.

import type { ReactNode } from "react";

const STATUS_LABEL: Record<string, string> = {
  new: "new",
  work: "work",
  human: "human",
  done: "done",
  cancel: "cancel",
  queued: "queued",
  running: "running",
  failed: "failed",
};

export function StatusBadge({ status, className }: { status: string; className?: string }) {
  const cls = `badge badge-${status}${className ? ` ${className}` : ""}`;
  return <span className={cls}>{STATUS_LABEL[status] ?? status}</span>;
}

export function PriorityDot({ priority }: { priority: number }) {
  const label = ["P0", "P1", "P2", "P3"][priority] ?? `P${priority}`;
  return <span className={`prio prio-${priority}`} title={`priority ${priority}`}>{label}</span>;
}

/** Format an RFC3339 UTC timestamp into the operator's local time. */
export function localTime(ts: string | null | undefined): string {
  if (!ts) return "";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

export function relativeTime(ts: string | null | undefined): string {
  if (!ts) return "";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "";
  const diff = Date.now() - d.getTime();
  const s = Math.round(diff / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  const days = Math.round(h / 24);
  return `${days}d ago`;
}

/** humanDuration — mirrors internal/lazy `humanDuration`: s/m/h/d buckets,
 * no "ago" suffix, negatives clamp to "0s". */
function humanDuration(ms: number): string {
  if (ms < 0) ms = 0;
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

/** jobWorkTime — mirrors internal/lazy `jobWorkTime`: how long pi actually
 * worked on a job. queued (no started_at) → "—"; finished → finished−started;
 * running → now−started. */
export function jobWorkTime(job: { started_at?: string | null; finished_at?: string | null }): string {
  if (!job.started_at) return "\u2014";
  const start = new Date(job.started_at).getTime();
  if (Number.isNaN(start)) return "\u2014";
  const endTs = job.finished_at ? new Date(job.finished_at).getTime() : Date.now();
  const end = Number.isNaN(endTs) ? Date.now() : endTs;
  return humanDuration(end - start);
}

export function Spinner({ label }: { label?: string }) {
  return (
    <div className="spinner">
      <span className="spinner-dot" /> {label ?? "Loading…"}
    </div>
  );
}

export function EmptyState({ title, hint }: { title: string; hint?: string }) {
  return (
    <div className="empty-state">
      <div className="empty-title">{title}</div>
      {hint && <div className="empty-hint">{hint}</div>}
    </div>
  );
}

export function Section({ title, children, action }: { title: string; children: ReactNode; action?: ReactNode }) {
  return (
    <div className="section">
      <div className="section-head">
        <span className="section-title">{title}</span>
        {action}
      </div>
      <div className="section-body">{children}</div>
    </div>
  );
}
