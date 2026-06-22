/**
 * The `autoskd mcp` toolset: `task` (create/update/show/list), `comment`
 * (add/list), and `transit` (ack-only). Exposed to Claude Code as
 * `mcp__autosk__task`, `mcp__autosk__comment`, and `mcp__autosk__transit`.
 *
 * `task` / `comment` are the direct analog of `@autosk/pi-tools`: the tool
 * schemas + descriptions + prompt guidance are copied from
 * `pi-tools/src/{task,comment}.ts`, and execution SHELLS OUT to `autosk … --json`
 * (reusing the ported `argv.ts` + `cli.ts`), returning the real store result.
 *
 * `transit` is the analog of pi-agent's `autosk_transit` tool: it only returns
 * an ack — the `@autosk/claude-agent` driver OBSERVES the `mcp__autosk__transit`
 * `tool_use` on the stream and drives the real `ctx.transit(...)`. It is gated
 * by `AUTOSK_MCP_TRANSIT=1` (set only in task mode), so an interactive chat is
 * never offered a tool that has nothing to transit.
 */

import {
  buildCommentAddArgv,
  buildCommentListArgv,
  buildTaskCreateArgv,
  buildTaskListArgv,
  buildTaskUpdateArgv,
  requireId,
} from "./argv.ts";
import { AutoskCliError, bunRunProcess, runAutoskJson, type RunProcess } from "./cli.ts";
import type { AutoskDetails, Comment, Task, TaskAction } from "./types.ts";

/** A JSON-Schema object (the subset MCP `inputSchema` needs). */
export type JsonSchema = Record<string, unknown>;

/** One MCP tool advertised by `tools/list` and dispatched by `tools/call`. */
export interface McpTool {
  name: string;
  description: string;
  inputSchema: JsonSchema;
}

/** A `tools/call` result (the MCP content + structured detail payload). */
export interface McpToolResult {
  content: { type: "text"; text: string }[];
  isError: boolean;
  /** Structured detail for richer clients (the `AutoskDetails` union). */
  structuredContent?: AutoskDetails;
}

/** The `{ action, args }` envelope both `task` and `comment` tool bodies take. */
export interface McpActionParams {
  action?: unknown;
  args?: unknown;
}

/**
 * The pluggable execution backend for the `task` / `comment` tools, so the same
 * transport-agnostic dispatch ({@link McpTool} list, `handleMessage`) serves two
 * very different homes:
 *  - the standalone `autoskd mcp` STDIO server SHELLS OUT to `autosk … --json`
 *    ({@link shellBackend}); and
 *  - the per-session HTTP MCP server runs DIRECT against the daemon's own store
 *    (`directStoreBackend`, no `autosk` child).
 * Both return the identical {@link McpToolResult} shape.
 */
export interface McpToolBackend {
  task(params: McpActionParams): Promise<McpToolResult>;
  comment(params: McpActionParams): Promise<McpToolResult>;
}

/** A {@link McpToolBackend} that shells out to `autosk … --json` (the stdio server path). */
export function shellBackend(run: RunProcess = bunRunProcess): McpToolBackend {
  return {
    task: (params) => callTask(params, run),
    comment: (params) => callComment(params, run),
  };
}

// ---------------------------------------------------------------------------
// transit (ack-only) — gated by AUTOSK_MCP_TRANSIT
// ---------------------------------------------------------------------------

export const TRANSIT_TOOL: McpTool = {
  name: "transit",
  description:
    "Record the workflow transition for the current autosk task. Call this exactly once, when you " +
    "are done with the step, with `to` set to the chosen sibling step name or one of done | cancel | human. " +
    "If a transition is rejected you will be told and may call this again with a different target.",
  inputSchema: {
    type: "object",
    properties: {
      to: {
        type: "string",
        description: "The transition target: a sibling step name, or one of done | cancel | human.",
      },
    },
    required: ["to"],
    additionalProperties: false,
  },
};

/** The transit tool body: ack only — the driver observes the call and transits. */
export function callTransit(args: Record<string, unknown>): McpToolResult {
  const to = String(args.to ?? "").trim();
  return {
    content: [
      {
        type: "text",
        text:
          to === ""
            ? "autosk: no transition target provided — call transit again with a `to`."
            : `autosk: transition to "${to}" submitted.`,
      },
    ],
    isError: false,
  };
}

// ---------------------------------------------------------------------------
// task (create / update / show / list)
// ---------------------------------------------------------------------------

const TASK_DESCRIPTION = `Manage autosk tasks (create / update / show / list) against the autosk daemon.
Use this tool for task reads and edits; prefer it over shelling out to \`autosk\` via bash.

Params: { action: "create"|"update"|"show"|"list", args: {...} }. Task ids look like "ask-XXXXXX".

create
  args.title (req)        — non-empty.
  args.description?       — free text.
  args.blocks?:  string[] — ids this new task will block.
  args.blocked_by?: string[] — ids that block this new task.
  args.workflow?: string  — enroll into a named workflow at its first step (status becomes 'work').

update
  args.id (req)           — target task.
  At least one of: title, description. (Status changes flow through the workflow transition, not here.)

show
  args.id (req)           — returns the task plus workflow / step / blocked / edges / comment_count.

list
  args.statuses?: string[] — filter (or ['all'] for no filter); default new, work, human.
  args.limit?: number      — cap rows.

Result: details = { kind: "task"|"tasks", domain: "task", action, ... }
or      details = { kind: "error", domain: "task", action, reason, message, ... }.
reason ∈ { missing_binary | invalid_args | cli_error | parse_error | aborted }.`;

export const TASK_TOOL: McpTool = {
  name: "task",
  description: TASK_DESCRIPTION,
  inputSchema: {
    type: "object",
    properties: {
      action: { type: "string", enum: ["create", "update", "show", "list"] },
      args: {
        type: "object",
        properties: {
          id: { type: "string", description: "Target task id (e.g. ask-a1b2c3). Required for show and update." },
          title: { type: "string", description: "Task title. Required by create; optional by update." },
          description: { type: "string", description: "Task description. Optional (create / update)." },
          blocks: {
            type: "array",
            items: { type: "string" },
            description: "create only: this new task will be a blocker for each of these ids.",
          },
          blocked_by: {
            type: "array",
            items: { type: "string" },
            description: "create only: each of these ids will block this new task.",
          },
          workflow: {
            type: "string",
            description: "create only: enroll the new task into this named workflow at its first step.",
          },
          statuses: {
            type: "array",
            items: { type: "string", enum: ["new", "work", "human", "done", "cancel", "all"] },
            description: "list only: filter by status (or a single 'all' = no filter). Default: new, work, human.",
          },
          limit: { type: "integer", minimum: 0, description: "list only: max rows (0 = unlimited)." },
        },
        additionalProperties: false,
      },
    },
    required: ["action"],
    additionalProperties: false,
  },
};

interface TaskArgs {
  id?: string;
  title?: string;
  description?: string;
  blocks?: string[];
  blocked_by?: string[];
  workflow?: string;
  statuses?: string[];
  limit?: number;
}

const TASK_DOMAIN = "task" as const;

export async function callTask(
  params: { action?: unknown; args?: unknown },
  run: RunProcess = bunRunProcess,
): Promise<McpToolResult> {
  const action = params.action as TaskAction;
  const args = (params.args ?? {}) as TaskArgs;
  try {
    if (action === "list") {
      const tasks = await runAutoskJson<Task[]>(run, TASK_DOMAIN, "list", buildTaskListArgv(args));
      const details: AutoskDetails = { kind: "tasks", domain: TASK_DOMAIN, action, tasks };
      return ok(formatListText(tasks), details);
    }
    let task: Task;
    if (action === "create") {
      task = await runAutoskJson<Task>(run, TASK_DOMAIN, "create", buildTaskCreateArgv(args));
    } else if (action === "update") {
      const id = requireId(TASK_DOMAIN, "update", args.id);
      task = await runAutoskJson<Task>(run, TASK_DOMAIN, "update", buildTaskUpdateArgv(id, args));
    } else if (action === "show") {
      const id = requireId(TASK_DOMAIN, "show", args.id);
      task = await runAutoskJson<Task>(run, TASK_DOMAIN, "show", ["show", id]);
    } else {
      throw new AutoskCliError(TASK_DOMAIN, String(action), "invalid_args", `unknown task action: ${String(action)}`);
    }
    const details: AutoskDetails = { kind: "task", domain: TASK_DOMAIN, action, task };
    return ok(formatTaskText(action, task), details);
  } catch (err) {
    return errorResult(TASK_DOMAIN, String(action), err);
  }
}

export function formatTaskText(action: TaskAction, task: Task): string {
  return `${action} ${formatTaskLine(task)}\n${JSON.stringify(task)}`;
}

export function formatListText(tasks: Task[]): string {
  if (tasks.length === 0) return "no tasks\n[]";
  const lines = tasks.map((t) => formatTaskLine(t));
  return `${tasks.length} task(s)\n${lines.join("\n")}\n${JSON.stringify(tasks)}`;
}

function formatTaskLine(t: Task): string {
  const blocked = t.blocked ? " blocked" : "";
  const wf = t.workflow ? ` [${t.workflow}${t.step ? `/${t.step}` : ""}]` : "";
  return `${t.id} ${t.status}${blocked}${wf} — ${t.title}`;
}

// ---------------------------------------------------------------------------
// comment (add / list)
// ---------------------------------------------------------------------------

const COMMENT_DESCRIPTION = `Append-only comments on autosk tasks. The workflow engine surfaces every prior comment at the top of each step's prompt, so this is the canonical cross-agent channel for a task.

Params: { action: "add"|"list", args: {...} }.

add
  args.task_id (req)  — task to attach the comment to.
  args.text    (req)  — non-empty comment body (any UTF-8, may be multi-line).
  args.author?        — override the author name; defaults to $AUTOSK_AGENT or 'human'.

list
  args.task_id (req)  — list comments on this task, oldest first.

Result:
- details.kind "comment"   → single new comment (add).
- details.kind "comments"  → ordered array (list).
- details.kind "error"     → { reason, message, stderr?, stdout? }.

Comments are immutable from a workflow agent's view — to correct one, append a new comment.`;

export const COMMENT_TOOL: McpTool = {
  name: "comment",
  description: COMMENT_DESCRIPTION,
  inputSchema: {
    type: "object",
    properties: {
      action: { type: "string", enum: ["add", "list"] },
      args: {
        type: "object",
        properties: {
          task_id: { type: "string", description: "Target task id (e.g. ask-a1b2c3). Required for add and list." },
          text: { type: "string", description: "Comment text. Required by add; non-empty." },
          author: {
            type: "string",
            description: "Optional author name override. Defaults to the AUTOSK_AGENT env value (or 'human').",
          },
        },
        additionalProperties: false,
      },
    },
    required: ["action"],
    additionalProperties: false,
  },
};

interface CommentArgs {
  task_id?: string;
  text?: string;
  author?: string;
}

const COMMENT_DOMAIN = "comment" as const;

export async function callComment(
  params: { action?: unknown; args?: unknown },
  run: RunProcess = bunRunProcess,
): Promise<McpToolResult> {
  const action = params.action === "add" ? "add" : "list";
  const args = (params.args ?? {}) as CommentArgs;
  try {
    if (action === "add") {
      const taskId = requireId(COMMENT_DOMAIN, "add", args.task_id);
      const text = (args.text ?? "").trim();
      if (text === "") {
        throw new AutoskCliError(COMMENT_DOMAIN, "add", "invalid_args", "add requires non-empty `text`");
      }
      const comment = await runAutoskJson<Comment>(
        run,
        COMMENT_DOMAIN,
        "add",
        buildCommentAddArgv(taskId, args.text ?? "", args.author),
      );
      const details: AutoskDetails = { kind: "comment", domain: COMMENT_DOMAIN, action, comment };
      return ok(formatAddText(taskId, comment), details);
    }
    const taskId = requireId(COMMENT_DOMAIN, "list", args.task_id);
    const comments = await runAutoskJson<Comment[]>(
      run,
      COMMENT_DOMAIN,
      "list",
      buildCommentListArgv(taskId),
    );
    const details: AutoskDetails = { kind: "comments", domain: COMMENT_DOMAIN, action: "list", task_id: taskId, comments };
    return ok(formatCommentListText(taskId, comments), details);
  } catch (err) {
    return errorResult(COMMENT_DOMAIN, action, err);
  }
}

export function formatAddText(taskId: string, c: Comment): string {
  return `added comment id=${c.id} on ${taskId} by ${c.author}\n${JSON.stringify(c)}`;
}

export function formatCommentListText(taskId: string, comments: Comment[]): string {
  if (comments.length === 0) return `${taskId}: no comments\n[]`;
  const lines = comments.map((c) => `[${c.id}] ${c.author} @ ${c.created_at}: ${oneLine(c.text)}`);
  return `${taskId}: ${comments.length} comment(s)\n${lines.join("\n")}\n${JSON.stringify(comments)}`;
}

function oneLine(text: string): string {
  return text.replace(/\s+/g, " ").slice(0, 200);
}

// ---------------------------------------------------------------------------
// shared
// ---------------------------------------------------------------------------

export function ok(text: string, details: AutoskDetails): McpToolResult {
  return { content: [{ type: "text", text }], isError: false, structuredContent: details };
}

export function errorResult(domain: "task" | "comment", action: string, err: unknown): McpToolResult {
  if (err instanceof AutoskCliError) {
    const details: AutoskDetails = {
      kind: "error",
      domain,
      action,
      reason: err.reason,
      message: err.message,
      stderr: err.stderr || undefined,
      stdout: err.stdout || undefined,
    };
    return { content: [{ type: "text", text: `[${err.reason}] ${err.message}` }], isError: true, structuredContent: details };
  }
  const message = err instanceof Error ? err.message : String(err);
  const details: AutoskDetails = { kind: "error", domain, action, reason: "cli_error", message };
  return { content: [{ type: "text", text: `[cli_error] ${message}` }], isError: true, structuredContent: details };
}
