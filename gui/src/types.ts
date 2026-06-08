// Wire-projection types — TypeScript mirrors of the serde structs in
// `crates/autosk-proto/src/wire.rs`. These are the JSON shapes autoskd returns
// over JSON-RPC. Field names are snake_case to match the wire exactly (the Rust
// side serialises with default serde naming). RFC3339 UTC for all timestamps
// (the machine-wire-format rule from AGENTS.md — the frontend renders local).
//
// Keep this file in lockstep with autosk-proto/src/wire.rs. The protocol golden
// tests (plan §8.2) guard the Rust side; this file is the client mirror.

/** A lightweight reference to a related task (`wire::TaskRef`). */
export interface TaskRef {
  id: string;
  status: string;
}

/** `task.get` / `task.list` / `task.ready` element (`wire::TaskView`). */
export interface TaskView {
  id: string;
  title: string;
  description: string;
  status: string;
  priority: number;
  author_id: string;
  author_name: string;
  workflow_id: string;
  workflow_name: string;
  current_step_id: string;
  step_name: string;
  agent_id: string;
  agent_name: string;
  blocked: boolean;
  blocked_by: TaskRef[];
  blocks: TaskRef[];
  comment_count: number;
  metadata: Record<string, unknown> | null;
  created_at: string;
  updated_at: string;
}

/** `job.get` / `job.list` element (`wire::Job`). */
export interface Job {
  job_id: string;
  task_id: string;
  step_id: string;
  status: string;
  transition_id?: number | null;
  pi_session_id?: string;
  session_path?: string;
  pid?: number | null;
  exit_code?: number | null;
  error?: string;
  corrections_used: number;
  max_corrections: number;
  created_at: string;
  started_at?: string | null;
  finished_at?: string | null;
  duration_ms: number;
  attach_count: number;
  streaming: boolean;
  workflow_name: string;
  step_name: string;
  agent_name: string;
}

/** One outgoing `step_transitions` row (`wire::WorkflowTransition`). */
export interface WorkflowTransition {
  id: number;
  next_step_id?: string;
  next_step_name?: string;
  task_status?: string;
  prompt_rule: string;
}

/** One row of a workflow's step graph (`wire::WorkflowStep`). */
export interface WorkflowStep {
  id: string;
  name: string;
  agent_id: string;
  agent_name: string;
  next_steps: string[];
  next_status: string[];
  task_count: number;
  max_visits: number;
  agent_params?: Record<string, unknown> | null;
  transitions?: WorkflowTransition[];
}

/** One non-terminal task referencing a workflow (`wire::NonTerminalTaskRef`). */
export interface NonTerminalTaskRef {
  id: string;
  status: string;
  step_name: string;
}

/** `workflow.list` / `workflow.get` element (`wire::Workflow`). */
export interface Workflow {
  id: string;
  name: string;
  description: string;
  is_synthetic: boolean;
  first_step: string;
  first_step_id: string;
  steps: WorkflowStep[];
  task_count: number;
  isolation: string;
  non_terminal_task_count: number;
  non_terminal_tasks: NonTerminalTaskRef[];
  created_at: string;
}

/** `agent.list` element (`wire::Agent`). */
export interface Agent {
  id: string;
  name: string;
  is_human: boolean;
  source: string;
  version: string;
  model: string;
  thinking: string;
  extra_args: string[];
  pi_skills: string[];
  pi_ext: string[];
  tasks_owned: number;
  created_at: string;
}

/** `comment.list` element (`wire::Comment`). */
export interface Comment {
  id: number;
  task_id: string;
  author_id: string;
  author_name: string;
  text: string;
  created_at: string;
}

/** `signal.forTask` / `signal.forJob` element (`wire::Signal`). */
export interface Signal {
  transition_id: number;
  task_id: string;
  job_id: string;
  step_id: string;
  step_name: string;
  workflow_id: string;
  workflow_name: string;
  target: string;
  agent_id: string;
  agent_name: string;
  created_at: string;
}

/** `job.messages` element (`wire::MessageEvent`). */
export interface MessageEvent {
  kind: string;
  ts?: string | null;
  text?: string;
  name?: string;
  input?: unknown;
  is_error?: boolean;
  raw?: unknown;
}

/** `healthz` per-project row (`wire::HealthProject`). */
export interface HealthProject {
  root: string;
  db_path: string;
  queued: number;
  running: number;
  opened_at: string;
}

/** `healthz` result (`wire::Health`). */
export interface Health {
  ok: boolean;
  workers: number;
  queued: number;
  running: number;
  db_path?: string;
  project_root?: string;
  projects?: HealthProject[];
}

/** `version` result (`wire::VersionInfo`). */
export interface VersionInfo {
  version: string;
  commit: string;
}

/** One `job-event` notification payload (`wire::JobEvent`). */
export interface JobEvent {
  kind: "message" | "status" | "done" | "error";
  job_id: string;
  event_id?: number;
  event?: MessageEvent | null;
  job?: Job | null;
  error?: string;
}

/** `project.list` / `project.add` element (`wire::ProjectInfo`). */
export interface ProjectInfo {
  root: string;
  db_path: string;
  name: string;
}

/** `workflow.updateIsolation` result (`wire::UpdateIsolationReport`). */
export interface UpdateIsolationReport {
  workflow: string;
  from: string;
  to: string;
  noop: boolean;
  dry_run: boolean;
  non_terminal_tasks?: string[];
  ensured_tasks?: EnsureRecord[];
  leftover_worktrees?: LeftoverWorktree[];
  rolled_back_ensures?: EnsureRecord[];
  failed_task?: string;
}

export interface EnsureRecord {
  task_id: string;
  path: string;
  branch: string;
  existing: boolean;
}

export interface LeftoverWorktree {
  task_id: string;
  path: string;
}

// ---- frontend-only types --------------------------------------------------

/** Backend transport mode (an app setting). */
export type BackendMode = "local" | "remote";

/** App settings persisted by the Tauri backend (`AppSettings` in Rust). */
export interface AppSettings {
  backend_mode: BackendMode;
  remote_host: string;
  remote_token: string;
}

/** Daemon connection status surfaced to the UI. */
export interface DaemonStatus {
  connected: boolean;
  mode: BackendMode;
  error?: string | null;
}

/** `task-changed` notification payload. */
export interface TaskChangedEvent {
  root: string;
  db_path: string;
}

/** The selector carried by every project-scoped RPC call. */
export interface Selector {
  cwd?: string;
  db_path?: string;
}

/** Reserved-status set helpers (mirror store.Status). */
export const OPEN_STATUSES = ["new", "work", "human"] as const;
export const TERMINAL_STATUSES = ["done", "cancel"] as const;
export type TaskStatus = "new" | "work" | "human" | "done" | "cancel";
