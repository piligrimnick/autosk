/**
 * Wire types mirroring autosk's v2 `--json` output. Keep aligned with:
 *   - internal/render.TaskJSON   (task wire shape)
 *   - cmd/autosk/comment.go      (commentJSON)
 *
 * autosk treats these shapes as stable across patch releases, so this file is
 * the canonical TS mirror. v2 dropped v1's `priority` / `author_id` /
 * `workflow_id` / `current_step` / `current_agent`; a task now carries
 * `workflow` / `step` (empty until enrolled) plus derived `blocked` / edges /
 * `comment_count`, and a comment id is a STRING with `updated_at`.
 */

export type TaskStatus = "new" | "work" | "human" | "done" | "cancel";

export interface Task {
	id: string;
	title: string;
	description: string;
	status: TaskStatus;
	/** Workflow the task is enrolled in, or "" when not enrolled. */
	workflow?: string;
	/** Current step within the workflow, or "" when not enrolled. */
	step?: string;
	created_at: string;
	updated_at: string;
	blocked: boolean;
	blocked_by: string[];
	blocks: string[];
	comment_count: number;
}

export interface Comment {
	id: string;
	author: string;
	text: string;
	created_at: string;
	updated_at: string;
}

/**
 * Discriminated union for renderer + LLM details. The `domain` field lets
 * renderers / event handlers route by tool family without parsing the action.
 */
export type AutoskDetails =
	| { kind: "task"; domain: "task"; action: TaskAction; task: Task }
	| { kind: "tasks"; domain: "task"; action: "list"; tasks: Task[] }
	| { kind: "comment"; domain: "comment"; action: "add"; comment: Comment }
	| { kind: "comments"; domain: "comment"; action: "list"; task_id: string; comments: Comment[] }
	| {
			kind: "error";
			domain: AutoskDomain;
			action: string;
			reason: AutoskErrorReason;
			message: string;
			stderr?: string;
			stdout?: string;
	  };

export type AutoskDomain = "task" | "comment";

export type TaskAction = "create" | "update" | "show" | "list";
export type CommentAction = "add" | "list";

export type AutoskErrorReason =
	| "missing_binary"
	| "invalid_args"
	| "cli_error"
	| "parse_error"
	| "aborted";
