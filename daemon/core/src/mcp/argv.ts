/**
 * Pure `autosk` argv builders + validation for the `autoskd mcp` server's
 * `task` / `comment` tools. Ported verbatim from `@autosk/pi-tools` `src/argv.ts`
 * so the MCP tools speak the exact same v2 CLI surface the pi tools do. Flag
 * names mirror the v2 CLI:
 *   - cmd/autosk/create.go, status_verbs.go (update), list.go
 *   - cmd/autosk/comment.go (add / list)
 */

import { AutoskCliError } from "./cli.ts";
import type { AutoskDomain } from "./types.ts";

export interface TaskCreateInput {
  title?: string;
  description?: string;
  blocks?: string[];
  blocked_by?: string[];
  workflow?: string;
}

export interface TaskUpdateInput {
  title?: string;
  description?: string;
}

export interface TaskListInput {
  statuses?: string[];
  limit?: number;
}

/** `autosk create <title> [--description …] [--blocks …] [--blocked-by …] [--workflow …]`. */
export function buildTaskCreateArgv(args: TaskCreateInput): string[] {
  const title = (args.title ?? "").trim();
  if (title === "") {
    throw new AutoskCliError("task", "create", "invalid_args", "create requires a non-empty `title`");
  }
  const argv: string[] = ["create", title];
  if (args.description !== undefined) argv.push("--description", args.description);
  for (const id of args.blocks ?? []) argv.push("--blocks", id);
  for (const id of args.blocked_by ?? []) argv.push("--blocked-by", id);
  if (args.workflow) argv.push("--workflow", args.workflow);
  return argv;
}

/** `autosk update <id> [--title …] [--description …]`. */
export function buildTaskUpdateArgv(id: string, args: TaskUpdateInput): string[] {
  const argv: string[] = ["update", id];
  let touched = false;
  if (args.title !== undefined) {
    argv.push("--title", args.title);
    touched = true;
  }
  if (args.description !== undefined) {
    argv.push("--description", args.description);
    touched = true;
  }
  if (!touched) {
    throw new AutoskCliError(
      "task",
      "update",
      "invalid_args",
      "update requires at least one of: title, description",
    );
  }
  return argv;
}

/** `autosk list [--status a,b] [--limit N]`. */
export function buildTaskListArgv(args: TaskListInput): string[] {
  const argv: string[] = ["list"];
  const statuses = (args.statuses ?? []).map((s) => s.trim()).filter((s) => s !== "");
  if (statuses.length > 0) argv.push("--status", statuses.join(","));
  if (args.limit !== undefined && args.limit > 0) argv.push("--limit", String(args.limit));
  return argv;
}

/** `autosk comment add <task-id> <text> [--author …]`. */
export function buildCommentAddArgv(taskId: string, text: string, author?: string): string[] {
  const argv: string[] = ["comment", "add", taskId, text];
  if (author) argv.push("--author", author);
  return argv;
}

/** `autosk comment list <task-id>`. */
export function buildCommentListArgv(taskId: string): string[] {
  return ["comment", "list", taskId];
}

/** Trims and validates a required task id, raising `invalid_args` otherwise. */
export function requireId(domain: AutoskDomain, action: string, raw: string | undefined): string {
  const id = (raw ?? "").trim();
  if (id === "") {
    throw new AutoskCliError(domain, action, "invalid_args", `${action} requires \`${idField(domain)}\``);
  }
  return id;
}

function idField(domain: AutoskDomain): string {
  return domain === "comment" ? "task_id" : "id";
}
