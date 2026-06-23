/**
 * Wire types mirroring autosk's v2 `--json` output, used by the `autoskd mcp`
 * stdio MCP server (the `task` / `comment` tools shell out to `autosk … --json`
 * and parse these shapes). Ported verbatim from `@autosk/pi-tools` `src/types.ts`
 * so the MCP tools return the exact same result shapes the pi tools do.
 *
 * Keep aligned with:
 *   - internal/render.TaskJSON   (task wire shape)
 *   - cmd/autosk/comment.go      (commentJSON)
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
 * Discriminated union for the tool result detail payload. The `domain` field
 * lets a client route by tool family without parsing the action.
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
