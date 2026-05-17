/**
 * Shape of the JSON object autosk emits for a single task (autosk show --json).
 * Kept aligned with autosk v0.1; the project documents this shape as stable
 * across patch releases, so this file is the canonical TS mirror.
 */
export interface Task {
	id: string;
	title: string;
	description: string;
	status: TaskStatus;
	priority: number;
	created_at: string;
	updated_at: string;
	blocked: boolean;
	blocked_by: string[];
	blocks: string[];
}

export type TaskStatus = "new" | "claimed" | "done" | "cancelled";

/** Discriminated union of details that the renderer / LLM consumes. */
export type AutoskDetails =
	| { kind: "task"; action: AutoskAction; task: Task }
	| { kind: "tasks"; action: AutoskAction; tasks: Task[] }
	| { kind: "ack"; action: AutoskAction; message: string; task?: Task }
	| { kind: "error"; action: AutoskAction; reason: AutoskErrorReason; message: string; stderr?: string; stdout?: string };

export type AutoskErrorReason =
	| "missing_binary"
	| "invalid_args"
	| "cli_error"
	| "parse_error"
	| "aborted";

/** Whitelisted actions the tool exposes to the agent. */
export type AutoskAction =
	| "create"
	| "show"
	| "update"
	| "claim"
	| "done"
	| "cancel"
	| "reopen"
	| "block"
	| "unblock"
	| "dep_list"
	| "list"
	| "ready"
	| "next"
	| "init";
