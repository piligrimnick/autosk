/**
 * The DIRECT-STORE {@link McpToolBackend} for the per-session HTTP MCP server
 * (plan §7). Where the standalone `autoskd mcp` stdio server SHELLS OUT to
 * `autosk … --json`, this runs the `task` / `comment` tools straight against the
 * daemon's own {@link Store} (and the engine for a workflow enroll), with NO
 * `autosk` child — so the in-sandbox harness needs neither the CLI nor a mounted
 * daemon socket.
 *
 * It returns the IDENTICAL {@link McpToolResult} shape the shell-out path does
 * (it reuses the same `format*` / `ok` / `errorResult` helpers and the same
 * `Task` / `Comment` wire shapes), so a client cannot tell the two apart. The
 * per-session binding fixes `{ author = step }` (the default comment/task author)
 * and the project `root`; explicit ids in args are honoured.
 */

import type { Comment as CommentView, TaskFilter, TaskStatus, TaskView } from "@autosk/sdk";

import type { Store } from "../store/store.ts";
import { requireId } from "./argv.ts";
import { AutoskCliError } from "./cli.ts";
import {
  errorResult,
  formatAddText,
  formatCommentListText,
  formatListText,
  formatTaskText,
  ok,
  type McpActionParams,
  type McpToolBackend,
  type McpToolResult,
} from "./tools.ts";
import type { AutoskDetails, Comment, Task, TaskAction } from "./types.ts";

/** Per-session binding for the direct-store backend. */
export interface StoreBackendBinding {
  /** The project store every tool call targets. */
  store: Store;
  /** Default author for `comment add` / created tasks (the running step). */
  author: string;
  /**
   * Enrolls a (freshly-created) task into a workflow, mirroring the CLI's
   * `create --workflow`. Bound to the engine + project root by the caller.
   */
  enroll: (id: string, target: { workflow: string }) => Promise<TaskView>;
}

const TASK_DOMAIN = "task" as const;
const COMMENT_DOMAIN = "comment" as const;

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

interface CommentArgs {
  task_id?: string;
  text?: string;
  author?: string;
}

/** Builds a {@link McpToolBackend} that runs `task` / `comment` against the store directly. */
export function directStoreBackend(binding: StoreBackendBinding): McpToolBackend {
  return {
    task: (params) => runTask(binding, params),
    comment: (params) => runComment(binding, params),
  };
}

// ---------------------------------------------------------------------------
// task (create / update / show / list)
// ---------------------------------------------------------------------------

async function runTask(binding: StoreBackendBinding, params: McpActionParams): Promise<McpToolResult> {
  const action = params.action as TaskAction;
  const args = (params.args ?? {}) as TaskArgs;
  const { store } = binding;
  try {
    if (action === "list") {
      const views = await store.listTaskViews(filterFor(args.statuses));
      const limited = args.limit && args.limit > 0 ? views.slice(0, args.limit) : views;
      const tasks = limited.map(toTask);
      const details: AutoskDetails = { kind: "tasks", domain: TASK_DOMAIN, action, tasks };
      return ok(formatListText(tasks), details);
    }
    let view: TaskView;
    if (action === "create") {
      const title = (args.title ?? "").trim();
      if (title === "") {
        throw new AutoskCliError(TASK_DOMAIN, "create", "invalid_args", "create requires a non-empty `title`");
      }
      view = await store.createTask({ title, description: args.description, blocked_by: args.blocked_by });
      // `blocks`: the new task becomes a blocker for each listed id.
      for (const blockedId of args.blocks ?? []) await store.block(blockedId, view.id);
      if (args.workflow) view = await binding.enroll(view.id, { workflow: args.workflow });
      else view = await store.taskView(view.id); // refresh derived `blocks`
    } else if (action === "update") {
      const id = requireId(TASK_DOMAIN, "update", args.id);
      if (args.title === undefined && args.description === undefined) {
        throw new AutoskCliError(
          TASK_DOMAIN,
          "update",
          "invalid_args",
          "update requires at least one of: title, description",
        );
      }
      view = await mapNotFound(TASK_DOMAIN, "update", () =>
        store.updateTask(id, { title: args.title, description: args.description }),
      );
    } else if (action === "show") {
      const id = requireId(TASK_DOMAIN, "show", args.id);
      view = await mapNotFound(TASK_DOMAIN, "show", () => store.taskView(id));
    } else {
      throw new AutoskCliError(TASK_DOMAIN, String(action), "invalid_args", `unknown task action: ${String(action)}`);
    }
    const task = toTask(view);
    const details: AutoskDetails = { kind: "task", domain: TASK_DOMAIN, action, task };
    return ok(formatTaskText(action, task), details);
  } catch (err) {
    return errorResult(TASK_DOMAIN, String(action), err);
  }
}

// ---------------------------------------------------------------------------
// comment (add / list)
// ---------------------------------------------------------------------------

async function runComment(binding: StoreBackendBinding, params: McpActionParams): Promise<McpToolResult> {
  const action = params.action === "add" ? "add" : "list";
  const args = (params.args ?? {}) as CommentArgs;
  const { store } = binding;
  try {
    if (action === "add") {
      const taskId = requireId(COMMENT_DOMAIN, "add", args.task_id);
      const text = (args.text ?? "").trim();
      if (text === "") {
        throw new AutoskCliError(COMMENT_DOMAIN, "add", "invalid_args", "add requires non-empty `text`");
      }
      const author = (args.author ?? "").trim() || binding.author;
      const comment = toComment(
        await mapNotFound(COMMENT_DOMAIN, "add", () => store.addComment(taskId, { author, text: args.text ?? "" })),
      );
      const details: AutoskDetails = { kind: "comment", domain: COMMENT_DOMAIN, action, comment };
      return ok(formatAddText(taskId, comment), details);
    }
    const taskId = requireId(COMMENT_DOMAIN, "list", args.task_id);
    const comments = (await mapNotFound(COMMENT_DOMAIN, "list", () => store.listComments(taskId))).map(toComment);
    const details: AutoskDetails = { kind: "comments", domain: COMMENT_DOMAIN, action: "list", task_id: taskId, comments };
    return ok(formatCommentListText(taskId, comments), details);
  } catch (err) {
    return errorResult(COMMENT_DOMAIN, action, err);
  }
}

// ---------------------------------------------------------------------------
// shape mapping + helpers (parity with the `autosk --json` wire shapes).
// ---------------------------------------------------------------------------

/** Maps a {@link TaskView} onto the mcp `Task` wire shape (matches `autosk … --json`). */
function toTask(v: TaskView): Task {
  return {
    id: v.id,
    title: v.title,
    description: v.description,
    status: v.status,
    workflow: v.workflow ?? "",
    step: v.step ?? "",
    created_at: v.created_at,
    updated_at: v.updated_at,
    blocked: v.blocked,
    blocked_by: v.blocked_by.map((r) => r.id),
    blocks: v.blocks.map((r) => r.id),
    comment_count: v.comment_count,
  };
}

/** Maps a store {@link CommentView} onto the mcp `Comment` wire shape. */
function toComment(c: CommentView): Comment {
  return { id: c.id, author: c.author, text: c.text, created_at: c.created_at, updated_at: c.updated_at };
}

/**
 * Builds the `task.list` filter from the tool's `statuses` arg, byte-matching the
 * CLI wire: a nil/empty list OR a single `"all"` sends NO status filter; any
 * other set filters by those statuses.
 */
function filterFor(statuses: string[] | undefined): TaskFilter | undefined {
  const cleaned = (statuses ?? []).map((s) => s.trim()).filter((s) => s !== "");
  if (cleaned.length === 0) return undefined;
  if (cleaned.length === 1 && cleaned[0]!.toLowerCase() === "all") return undefined;
  return { status: cleaned as TaskStatus[] };
}

/**
 * Maps a store error (e.g. plain `task not found`) onto a `cli_error`-shaped tool
 * error — the parity-correct reason for the `autosk --json` path (whose tool
 * surface only ever surfaces `cli_error` for a store-level failure), so the
 * direct-store backend matches it.
 */
async function mapNotFound<T>(domain: "task" | "comment", action: string, fn: () => Promise<T>): Promise<T> {
  try {
    return await fn();
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new AutoskCliError(domain, action, "cli_error", msg);
  }
}
