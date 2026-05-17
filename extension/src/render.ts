import type { ExtensionContext } from "@earendil-works/pi-coding-agent";
import { type Component, Text, truncateToWidth, visibleWidth } from "@earendil-works/pi-tui";
import type { AutoskArgs, AutoskParams } from "./schema.ts";
import type { AutoskAction, AutoskDetails, Task, TaskStatus } from "./types.ts";

type ToolTheme = ExtensionContext["ui"]["theme"];

const TITLE_TRUNCATE_WIDTH = 60;
const ID_WIDTH = 9; // "as-XXXX" + small margin

// ---------------------------------------------------------------------------
// renderCall — single-line "autosk <action> ..." with the most relevant arg.
// ---------------------------------------------------------------------------

export function renderAutoskCall(args: unknown, theme: ToolTheme): Component {
	const params = (args ?? {}) as AutoskParams;
	const action = (params.action ?? "?") as AutoskAction | "?";
	const argSummary = summarizeArgs(action, params.args ?? {});
	let line = theme.fg("toolTitle", theme.bold(`autosk ${action}`));
	if (argSummary) {
		line += " " + theme.fg("dim", truncateToWidth(argSummary, 80));
	}
	return new Text(line, 0, 0);
}

function summarizeArgs(action: AutoskAction | "?", args: AutoskArgs): string {
	switch (action) {
		case "create":
			return args.title ? `"${args.title}"` + priorityHint(args) : priorityHint(args);
		case "show":
		case "done":
		case "cancel":
		case "reopen":
		case "update":
		case "dep_list":
			return args.id ?? "";
		case "step_next":
			if (!args.id) return "";
			return args.to ? `${args.id} --to ${args.to}` : args.id;
		case "block":
			return args.id ? `${args.id} ← [${(args.blocker_ids ?? []).join(", ")}]` : "";
		case "unblock":
			if (!args.id) return "";
			return args.all ? `${args.id} --all` : `${args.id} ← [${(args.blocker_ids ?? []).join(", ")}]`;
		case "list": {
			const parts: string[] = [];
			if (args.status_filter) parts.push(`status=${args.status_filter}`);
			if (args.priority_filter !== undefined) parts.push(`p=${args.priority_filter}`);
			if (args.limit !== undefined) parts.push(`limit=${args.limit}`);
			return parts.join(" ");
		}
		case "ready":
			return args.limit !== undefined ? `limit=${args.limit}` : "";
		case "next":
			return "";
		case "init":
			return args.prefix ? `--prefix ${args.prefix}` : "";
		default:
			return "";
	}
}

function priorityHint(args: AutoskArgs): string {
	return args.priority !== undefined ? ` p=${args.priority}` : "";
}

// ---------------------------------------------------------------------------
// renderResult — compact rendering specific to the details kind.
// ---------------------------------------------------------------------------

interface ToolResult {
	content: Array<{ type?: string; text?: string }>;
	details?: AutoskDetails;
}

export function renderAutoskResult(
	result: ToolResult,
	_options: unknown,
	theme: ToolTheme,
): Component {
	const details = result.details;
	if (!details) {
		return new Text(fallbackText(result), 0, 0);
	}
	if (details.kind === "error") {
		return new Text(renderError(details, theme), 0, 0);
	}
	if (details.kind === "ack") {
		return new Text(theme.fg("muted", details.message), 0, 0);
	}
	if (details.kind === "task") {
		return new Text(renderTaskBlock(details.action, details.task, theme), 0, 0);
	}
	return new Text(renderTaskTable(details.action, details.tasks, theme), 0, 0);
}

function renderError(
	details: Extract<AutoskDetails, { kind: "error" }>,
	theme: ToolTheme,
): string {
	const head = theme.fg("error", `autosk ${details.action} — ${details.reason}`);
	const body = theme.fg("muted", details.message);
	return `${head}\n${body}`;
}

function renderTaskBlock(action: AutoskAction, task: Task, theme: ToolTheme): string {
	const head = `${theme.bold(theme.fg("accent", task.id))}  ${statusBadge(task.status, theme)}  ${priorityBadge(task.priority, theme)}${
		task.blocked ? "  " + theme.fg("warning", "blocked") : ""
	}`;
	const titleLine = theme.fg("text", task.title || theme.fg("dim", "(no title)"));
	const lines = [head, titleLine];
	if (task.description?.trim()) {
		lines.push(theme.fg("dim", truncateToWidth(task.description.replace(/\s+/g, " "), 200)));
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

function renderTaskTable(action: AutoskAction, tasks: Task[], theme: ToolTheme): string {
	if (tasks.length === 0) {
		return theme.fg("muted", `${action}: no tasks`);
	}
	const header = `${pad("ID", ID_WIDTH)}  P  STATUS     TITLE`;
	const lines: string[] = [theme.fg("dim", header)];
	for (const t of tasks) {
		const id = pad(t.id, ID_WIDTH);
		const prio = String(t.priority);
		const status = pad(t.status, 9);
		const title = truncateToWidth(t.title || "(no title)", TITLE_TRUNCATE_WIDTH);
		const blockedMark = t.blocked ? theme.fg("warning", " ⛔") : "";
		const line = `${theme.fg("accent", id)}  ${theme.fg("muted", prio)}  ${theme.fg(statusColor(t.status), status)}  ${title}${blockedMark}`;
		lines.push(line);
	}
	return lines.join("\n");
}

function statusBadge(status: TaskStatus, theme: ToolTheme): string {
	return theme.fg(statusColor(status), status);
}

function priorityBadge(priority: number, theme: ToolTheme): string {
	return theme.fg("muted", `p=${priority}`);
}

function statusColor(status: TaskStatus): "success" | "warning" | "muted" | "text" {
	switch (status) {
		case "done":
			return "success";
		case "in_workflow":
			return "warning";
		case "human_feedback":
			return "warning";
		case "cancelled":
			return "muted";
		case "new":
			return "text";
	}
}

function pad(value: string, width: number): string {
	const w = visibleWidth(value);
	if (w >= width) return value;
	return value + " ".repeat(width - w);
}

function fallbackText(result: ToolResult): string {
	const first = result.content[0];
	if (first && first.type === "text" && typeof first.text === "string") {
		return first.text;
	}
	return "";
}
