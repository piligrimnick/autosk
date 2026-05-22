import type {
	ExtensionAPI,
	ExtensionContext,
} from "@earendil-works/pi-coding-agent";
import { type Static, Type } from "typebox";
import { type Component, Text, truncateToWidth } from "@earendil-works/pi-tui";

import { AutoskCliError, runAutoskJson, type RunOptions } from "./cli.ts";
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
]);

const TaskStatusSchema = Type.Union([
	Type.Literal("new"),
	Type.Literal("work"),
	Type.Literal("human"),
	Type.Literal("done"),
	Type.Literal("cancel"),
]);

const TaskArgsSchema = Type.Partial(
	Type.Object({
		/** Target task id, e.g. "ask-a1b2c3". Required for show / update. */
		id: Type.String({ description: "Target task id (e.g. ask-a1b2c3). Required for show and update." }),
		/** Task title. create / update. */
		title: Type.String({ description: "Task title. Required by create; optional by update." }),
		/** Task description. create / update. */
		description: Type.String({ description: "Task description. Optional." }),
		/** Priority 0..3 (0=highest). create / update. */
		priority: Type.Integer({
			minimum: 0,
			maximum: 3,
			description: "Priority 0..3 (0 = highest). Default 2 on create.",
		}),
		/** Target status. update only — any → any (with engine-enforced rules). */
		status: TaskStatusSchema,
		/** create only: ids this new task will block. */
		blocks: Type.Array(Type.String(), {
			description: "create only: this new task will be a blocker for each of these ids.",
		}),
		/** create only: ids that block this new task. */
		blocked_by: Type.Array(Type.String(), {
			description: "create only: each of these ids will block this new task.",
		}),
		/** create only: join an existing workflow at its first step. */
		workflow: Type.String({
			description: "create only: join this named workflow at its first step (status becomes work).",
		}),
		/** create only: shorthand for workflow=single:<agent>. */
		agent: Type.String({
			description: "create only: shorthand for workflow=single:<agent>; mutually exclusive with `workflow`.",
		}),
		/** create only: start at a specific step (requires `workflow`). */
		step: Type.String({
			description: "create only: enter the workflow at this step name instead of first_step. Requires `workflow`.",
		}),
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

const TOOL_DESCRIPTION = `Manage autosk tasks (create / update / show) against ./.autosk/db.
Use this tool for any task mutation; never edit ./.autosk/db by hand and prefer this over shelling out to \`autosk\` via bash.

Params: { action: "create"|"update"|"show", args: {...} }. Task ids look like "ask-XXXXXX".

create
  args.title (req)                — non-empty.
  args.description?               — free text.
  args.priority? (0..3, default 2) — 0 is highest.
  args.blocks?:  string[]         — ids this new task will block.
  args.blocked_by?: string[]      — ids that block this new task.
  args.workflow?: string          — join an existing workflow at its first step.
  args.agent?:    string          — shorthand for workflow=single:<agent>. Mutually exclusive with workflow.
  args.step?:     string          — enter the workflow at a specific step name (requires workflow).

update
  args.id (req) — target task.
  At least one of: title, description, priority, status. Status changes on work tasks are rejected by the engine; advance them via the workflow's step_next instead.

show
  args.id (req) — returns the task plus blocked_by / blocks / blocked / workflow_id / current_step / current_agent.

Result: details = { kind: "task", domain: "task", action, task }
or      details = { kind: "error", domain: "task", action, reason, message, ... }.
reason ∈ { missing_binary | invalid_args | cli_error | parse_error | aborted }.

Operates relative to the agent's cwd; autosk walks up to find .autosk/db (or auto-creates one on first write).`;

const TOOL_PROMPT_SNIPPET =
	"Create / update / show autosk tasks in ./.autosk/db with structured args.";

const TOOL_PROMPT_GUIDELINES = [
	"Use `autosk_task` for any task mutation — never bash to the `autosk` CLI.",
	"When `autosk_task` returns kind:'task', trust the returned task object; do not re-issue a show afterwards.",
	"On `autosk_task` action:'create', set `workflow` / `agent` only if you actually want the task to enter a workflow immediately; otherwise leave them unset and the task starts in status='new'.",
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
	const args: TaskArgs = params.args ?? {};
	const runOptions: RunOptions = { cwd: ctx.cwd, signal };
	const action = params.action;
	try {
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
	action: TaskAction,
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
	const title = (args.title ?? "").trim();
	if (title === "") {
		throw new AutoskCliError(DOMAIN, "create", "invalid_args", "create requires a non-empty `title`");
	}
	if (args.workflow && args.agent) {
		throw new AutoskCliError(
			DOMAIN,
			"create",
			"invalid_args",
			"`workflow` and `agent` are mutually exclusive on create",
		);
	}
	if (args.step && !args.workflow) {
		throw new AutoskCliError(
			DOMAIN,
			"create",
			"invalid_args",
			"`step` requires `workflow` (single:<agent> workflows have a single step)",
		);
	}

	const argv: string[] = ["create", title];
	if (args.description !== undefined) argv.push("--description", args.description);
	if (args.priority !== undefined) argv.push("--priority", String(args.priority));
	for (const id of args.blocks ?? []) argv.push("--blocks", id);
	for (const id of args.blocked_by ?? []) argv.push("--blocked-by", id);
	if (args.workflow) argv.push("--workflow", args.workflow);
	if (args.agent) argv.push("--agent", args.agent);
	if (args.step) argv.push("--step", args.step);

	return runAutoskJson<Task>(pi, DOMAIN, "create", argv, runOptions);
}

async function runUpdate(
	pi: Pick<ExtensionAPI, "exec">,
	args: TaskArgs,
	runOptions: RunOptions,
): Promise<Task> {
	const id = requireId("update", args);
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
	if (args.priority !== undefined) {
		argv.push("--priority", String(args.priority));
		touched = true;
	}
	if (args.status !== undefined) {
		argv.push("--status", args.status);
		touched = true;
	}
	if (!touched) {
		throw new AutoskCliError(
			DOMAIN,
			"update",
			"invalid_args",
			"update requires at least one of: title, description, priority, status",
		);
	}
	return runAutoskJson<Task>(pi, DOMAIN, "update", argv, runOptions);
}

async function runShow(
	pi: Pick<ExtensionAPI, "exec">,
	args: TaskArgs,
	runOptions: RunOptions,
): Promise<Task> {
	const id = requireId("show", args);
	return runAutoskJson<Task>(pi, DOMAIN, "show", ["show", id], runOptions);
}

function requireId(action: TaskAction, args: TaskArgs): string {
	const id = (args.id ?? "").trim();
	if (id === "") {
		throw new AutoskCliError(DOMAIN, action, "invalid_args", `${action} requires \`id\``);
	}
	return id;
}

// ---------------------------------------------------------------------------
// formatting (LLM-facing text)
// ---------------------------------------------------------------------------

function formatTaskText(action: TaskAction, task: Task): string {
	return `${action} ${formatTaskLine(task)}\n${JSON.stringify(task)}`;
}

function formatTaskLine(t: Task): string {
	const blocked = t.blocked ? " blocked" : "";
	return `${t.id} p=${t.priority} ${t.status}${blocked} — ${t.title}`;
}

function normalizeError(action: TaskAction, err: unknown): {
	text: string;
	details: AutoskDetails;
} {
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
			if (args.priority !== undefined) parts.push(`p=${args.priority}`);
			if (args.workflow) parts.push(`workflow=${args.workflow}`);
			if (args.agent) parts.push(`agent=${args.agent}`);
			if (args.step) parts.push(`step=${args.step}`);
			return parts.join(" ");
		}
		case "update": {
			const parts: string[] = [];
			if (args.id) parts.push(args.id);
			const touched: string[] = [];
			if (args.title !== undefined) touched.push("title");
			if (args.description !== undefined) touched.push("description");
			if (args.priority !== undefined) touched.push(`p=${args.priority}`);
			if (args.status !== undefined) touched.push(`status=${args.status}`);
			if (touched.length > 0) parts.push(`[${touched.join(",")}]`);
			return parts.join(" ");
		}
		case "show":
			return args.id ?? "";
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
	const head = `${theme.bold(theme.fg("accent", task.id))}  ${statusBadge(task.status, theme)}  ${theme.fg("muted", `p=${task.priority}`)}${
		task.blocked ? "  " + theme.fg("warning", "blocked") : ""
	}`;
	const lines = [head];
	lines.push(theme.fg("text", task.title || theme.fg("dim", "(no title)")));
	if (task.description?.trim()) {
		lines.push(theme.fg("dim", truncateToWidth(task.description.replace(/\s+/g, " "), 200)));
	}
	if (task.workflow_id) {
		const wf = `workflow: ${task.workflow_id}`;
		const step = task.current_step ? ` · step=${task.current_step}` : "";
		const agent = task.current_agent ? ` · agent=${task.current_agent}` : "";
		lines.push(theme.fg("muted", `${wf}${step}${agent}`));
	}
	if (task.blocked_by.length > 0) {
		lines.push(theme.fg("muted", `blocked_by: ${task.blocked_by.join(", ")}`));
	}
	if (task.blocks.length > 0) {
		lines.push(theme.fg("muted", `blocks:     ${task.blocks.join(", ")}`));
	}
	lines.push(theme.fg("dim", `action: ${action}`));
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

