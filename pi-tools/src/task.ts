import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { type Static, Type } from "typebox";
import { type Component, Text, truncateToWidth } from "@earendil-works/pi-tui";

import { AutoskCliError, runAutoskJson, type RunOptions } from "./cli.ts";
import { mcpCall, mcpEnabled } from "./mcp.ts";
import {
	buildTaskCreateArgv,
	buildTaskListArgv,
	buildTaskUpdateArgv,
	requireId,
} from "./argv.ts";
import type { AutoskDetails, Task, TaskAction, TaskStatus } from "./types.ts";

type ToolTheme = ExtensionContext["ui"]["theme"];

const DOMAIN = "task" as const;

// ---------------------------------------------------------------------------
// schema
// ---------------------------------------------------------------------------

const TaskActionSchema = Type.Union([
	Type.Literal("create"),
	Type.Literal("update"),
	Type.Literal("show"),
	Type.Literal("list"),
]);

const TaskStatusSchema = Type.Union([
	Type.Literal("new"),
	Type.Literal("work"),
	Type.Literal("human"),
	Type.Literal("done"),
	Type.Literal("cancel"),
	Type.Literal("all"),
]);

const TaskArgsSchema = Type.Partial(
	Type.Object({
		id: Type.String({ description: "Target task id (e.g. ask-a1b2c3). Required for show and update." }),
		title: Type.String({ description: "Task title. Required by create; optional by update." }),
		description: Type.String({ description: "Task description. Optional (create / update)." }),
		blocks: Type.Array(Type.String(), {
			description: "create only: this new task will be a blocker for each of these ids.",
		}),
		blocked_by: Type.Array(Type.String(), {
			description: "create only: each of these ids will block this new task.",
		}),
		workflow: Type.String({
			description: "create only: enroll the new task into this named workflow at its first step.",
		}),
		statuses: Type.Array(TaskStatusSchema, {
			description: "list only: filter by status (or a single 'all' = no filter). Default: new, work, human.",
		}),
		limit: Type.Integer({ minimum: 0, description: "list only: max rows (0 = unlimited)." }),
	}),
);

export const TaskParamsSchema = Type.Object({
	action: TaskActionSchema,
	args: Type.Optional(TaskArgsSchema),
});

export type TaskArgs = Static<typeof TaskArgsSchema>;
export type TaskParams = Static<typeof TaskParamsSchema>;

// ---------------------------------------------------------------------------
// tool description (LLM-visible)
// ---------------------------------------------------------------------------

const TOOL_DESCRIPTION = `Manage autosk tasks (create / update / show / list) against the autosk daemon.
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

const TOOL_PROMPT_SNIPPET = "Create / update / show / list autosk tasks via the autosk daemon.";

const TOOL_PROMPT_GUIDELINES = [
	"Use `autosk_task` for task reads/edits — prefer it over bashing the `autosk` CLI.",
	"When `autosk_task` returns kind:'task', trust the returned task object; do not re-issue a show afterwards.",
	"`autosk_task` does not change status: advance an enrolled task through its workflow transition instead.",
] as const;

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

export function registerTaskTool(pi: ExtensionAPI) {
	pi.registerTool({
		name: "autosk_task",
		label: "autosk task",
		description: TOOL_DESCRIPTION,
		promptSnippet: TOOL_PROMPT_SNIPPET,
		promptGuidelines: [...TOOL_PROMPT_GUIDELINES],
		parameters: TaskParamsSchema,
		execute: (_callId, params, signal, _onUpdate, ctx) =>
			executeTaskTool(pi, params as TaskParams, signal, ctx),
		renderCall: renderTaskCall,
		renderResult: renderTaskResult,
	});
}

async function executeTaskTool(
	pi: Pick<ExtensionAPI, "exec">,
	params: TaskParams,
	signal: AbortSignal | undefined,
	ctx: ExtensionContext,
) {
	// Transport seam: in a thin sandbox (AUTOSK_MCP_URL set) route through the
	// per-session HTTP MCP server; on the host fall back to the `autosk` CLI.
	if (mcpEnabled()) return mcpCall(DOMAIN, params, signal);
	const args: TaskArgs = params.args ?? {};
	const runOptions: RunOptions = { cwd: ctx.cwd, signal };
	const action = params.action;
	try {
		if (action === "list") {
			const tasks = await runList(pi, args, runOptions);
			const details: AutoskDetails = { kind: "tasks", domain: DOMAIN, action, tasks };
			return {
				content: [{ type: "text" as const, text: formatListText(tasks) }],
				details,
				isError: false,
			};
		}
		const task = await dispatch(pi, action, args, runOptions);
		const details: AutoskDetails = { kind: "task", domain: DOMAIN, action, task };
		return {
			content: [{ type: "text" as const, text: formatTaskText(action, task) }],
			details,
			isError: false,
		};
	} catch (err) {
		const norm = normalizeError(action, err);
		return {
			content: [{ type: "text" as const, text: norm.text }],
			details: norm.details,
			isError: true,
		};
	}
}

// ---------------------------------------------------------------------------
// action runners
// ---------------------------------------------------------------------------

async function dispatch(
	pi: Pick<ExtensionAPI, "exec">,
	action: Exclude<TaskAction, "list">,
	args: TaskArgs,
	runOptions: RunOptions,
): Promise<Task> {
	switch (action) {
		case "create":
			return runCreate(pi, args, runOptions);
		case "update":
			return runUpdate(pi, args, runOptions);
		case "show":
			return runShow(pi, args, runOptions);
	}
}

async function runCreate(
	pi: Pick<ExtensionAPI, "exec">,
	args: TaskArgs,
	runOptions: RunOptions,
): Promise<Task> {
	return runAutoskJson<Task>(pi, DOMAIN, "create", buildTaskCreateArgv(args), runOptions);
}

async function runUpdate(
	pi: Pick<ExtensionAPI, "exec">,
	args: TaskArgs,
	runOptions: RunOptions,
): Promise<Task> {
	const id = requireId(DOMAIN, "update", args.id);
	return runAutoskJson<Task>(pi, DOMAIN, "update", buildTaskUpdateArgv(id, args), runOptions);
}

async function runShow(
	pi: Pick<ExtensionAPI, "exec">,
	args: TaskArgs,
	runOptions: RunOptions,
): Promise<Task> {
	const id = requireId(DOMAIN, "show", args.id);
	return runAutoskJson<Task>(pi, DOMAIN, "show", ["show", id], runOptions);
}

async function runList(
	pi: Pick<ExtensionAPI, "exec">,
	args: TaskArgs,
	runOptions: RunOptions,
): Promise<Task[]> {
	return runAutoskJson<Task[]>(pi, DOMAIN, "list", buildTaskListArgv(args), runOptions);
}

// ---------------------------------------------------------------------------
// formatting (LLM-facing text)
// ---------------------------------------------------------------------------

function formatTaskText(action: TaskAction, task: Task): string {
	return `${action} ${formatTaskLine(task)}\n${JSON.stringify(task)}`;
}

function formatListText(tasks: Task[]): string {
	if (tasks.length === 0) return "no tasks\n[]";
	const lines = tasks.map((t) => formatTaskLine(t));
	return `${tasks.length} task(s)\n${lines.join("\n")}\n${JSON.stringify(tasks)}`;
}

function formatTaskLine(t: Task): string {
	const blocked = t.blocked ? " blocked" : "";
	const wf = t.workflow ? ` [${t.workflow}${t.step ? `/${t.step}` : ""}]` : "";
	return `${t.id} ${t.status}${blocked}${wf} — ${t.title}`;
}

function normalizeError(
	action: TaskAction,
	err: unknown,
): { text: string; details: AutoskDetails } {
	if (err instanceof AutoskCliError) {
		return {
			text: `[${err.reason}] ${err.message}`,
			details: {
				kind: "error",
				domain: DOMAIN,
				action,
				reason: err.reason,
				message: err.message,
				stderr: err.stderr || undefined,
				stdout: err.stdout || undefined,
			},
		};
	}
	throw err;
}

// ---------------------------------------------------------------------------
// renderers (TUI)
// ---------------------------------------------------------------------------

function renderTaskCall(args: unknown, theme: ToolTheme): Component {
	const params = (args ?? {}) as TaskParams;
	const action = (params.action ?? "?") as TaskAction | "?";
	const summary = summarizeArgs(action, params.args ?? {});
	let line = theme.fg("toolTitle", theme.bold(`autosk_task ${action}`));
	if (summary) line += " " + theme.fg("dim", truncateToWidth(summary, 80));
	return new Text(line, 0, 0);
}

function summarizeArgs(action: TaskAction | "?", args: TaskArgs): string {
	switch (action) {
		case "create": {
			const parts: string[] = [];
			if (args.title) parts.push(`"${args.title}"`);
			if (args.workflow) parts.push(`workflow=${args.workflow}`);
			return parts.join(" ");
		}
		case "update": {
			const parts: string[] = [];
			if (args.id) parts.push(args.id);
			const touched: string[] = [];
			if (args.title !== undefined) touched.push("title");
			if (args.description !== undefined) touched.push("description");
			if (touched.length > 0) parts.push(`[${touched.join(",")}]`);
			return parts.join(" ");
		}
		case "show":
			return args.id ?? "";
		case "list":
			return (args.statuses ?? []).join(",");
		default:
			return "";
	}
}

interface ToolResult {
	content: Array<{ type?: string; text?: string }>;
	details?: AutoskDetails;
}

function renderTaskResult(result: ToolResult, _options: unknown, theme: ToolTheme): Component {
	const details = result.details;
	if (!details) return new Text(fallbackText(result), 0, 0);
	if (details.kind === "error") return new Text(renderError(details, theme), 0, 0);
	if (details.kind === "task") return new Text(renderTaskBlock(details.action, details.task, theme), 0, 0);
	if (details.kind === "tasks") return new Text(renderTaskList(details.tasks, theme), 0, 0);
	return new Text(fallbackText(result), 0, 0);
}

function renderError(
	details: Extract<AutoskDetails, { kind: "error" }>,
	theme: ToolTheme,
): string {
	const head = theme.fg("error", `autosk_${details.domain} ${details.action} — ${details.reason}`);
	const body = theme.fg("muted", details.message);
	return `${head}\n${body}`;
}

function renderTaskBlock(action: TaskAction, task: Task, theme: ToolTheme): string {
	const head = `${theme.bold(theme.fg("accent", task.id))}  ${statusBadge(task.status, theme)}${
		task.blocked ? "  " + theme.fg("warning", "blocked") : ""
	}`;
	const lines = [head];
	lines.push(theme.fg("text", task.title || theme.fg("dim", "(no title)")));
	if (task.description?.trim()) {
		lines.push(theme.fg("dim", truncateToWidth(task.description.replace(/\s+/g, " "), 200)));
	}
	if (task.workflow) {
		const step = task.step ? ` · step=${task.step}` : "";
		lines.push(theme.fg("muted", `workflow: ${task.workflow}${step}`));
	}
	if (task.blocked_by.length > 0) lines.push(theme.fg("muted", `blocked_by: ${task.blocked_by.join(", ")}`));
	if (task.blocks.length > 0) lines.push(theme.fg("muted", `blocks:     ${task.blocks.join(", ")}`));
	lines.push(theme.fg("dim", `action: ${action} · comments: ${task.comment_count}`));
	return lines.join("\n");
}

function renderTaskList(tasks: Task[], theme: ToolTheme): string {
	if (tasks.length === 0) return theme.fg("muted", "no tasks");
	const lines: string[] = [theme.fg("dim", `${tasks.length} task(s)`)];
	for (const t of tasks) {
		const id = theme.bold(theme.fg("accent", t.id));
		const badge = statusBadge(t.status, theme);
		const title = truncateToWidth(t.title.replace(/\n/g, " "), 80);
		lines.push(`${id} ${badge}  ${title}`);
	}
	return lines.join("\n");
}

function statusBadge(status: TaskStatus, theme: ToolTheme): string {
	return theme.fg(statusColor(status), status);
}

function statusColor(status: TaskStatus): "success" | "warning" | "muted" | "text" {
	switch (status) {
		case "done":
			return "success";
		case "work":
		case "human":
			return "warning";
		case "cancel":
			return "muted";
		case "new":
			return "text";
	}
}

function fallbackText(result: ToolResult): string {
	const first = result.content[0];
	if (first && first.type === "text" && typeof first.text === "string") return first.text;
	return "";
}
