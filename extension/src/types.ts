/**
 * Wire types mirroring autosk's JSON output. Keep aligned with:
 *   - internal/render.TaskJSON      (task wire shape)
 *   - cmd/autosk/comment.go         (commentJSON)
 *   - cmd/autosk/step.go            (stepSignalJSON)
 *
 * autosk treats these shapes as stable across patch releases, so this
 * file is the canonical TS mirror.
 */

export type TaskStatus =
	| "new"
	| "work"
	| "human"
	| "done"
	| "cancel";

export interface Task {
	id: string;
	title: string;
	description: string;
	status: TaskStatus;
	priority: number;
	author_id?: string;
	workflow_id?: string;
	current_step?: string;
	current_agent?: string;
	created_at: string;
	updated_at: string;
	blocked: boolean;
	blocked_by: string[];
	blocks: string[];
}

export interface Comment {
	id: number;
	task_id: string;
	author_id: string;
	author: string;
	text: string;
	created_at: string;
}

export interface StepSignal {
	run_id: string;
	task_id: string;
	transition_id: number;
	next_step?: string;
	task_status?: string;
	prompt_rule: string;
	created_at: string;
}

/**
 * Discriminated union for renderer + LLM details.
 * The `domain` field lets renderers / event handlers route by tool family
 * without parsing the action string.
 */
export type AutoskDetails =
	| { kind: "task"; domain: "task"; action: TaskAction; task: Task }
	| { kind: "comment"; domain: "comment"; action: "add"; comment: Comment }
	| { kind: "comments"; domain: "comment"; action: "list"; task_id: string; comments: Comment[] }
	| { kind: "step_signal"; domain: "step"; action: "next"; signal: StepSignal }
	| {
			kind: "error";
			domain: AutoskDomain;
			action: string;
			reason: AutoskErrorReason;
			message: string;
			stderr?: string;
			stdout?: string;
	  };

export type AutoskDomain = "task" | "comment" | "step";

export type TaskAction = "create" | "update" | "show";
export type CommentAction = "add" | "list";
export type StepAction = "next";

export type AutoskErrorReason =
	| "missing_binary"
	| "invalid_args"
	| "cli_error"
	| "parse_error"
	| "aborted";
