import type {
	ExtensionAPI,
	ExtensionContext,
} from "@earendil-works/pi-coding-agent";
import { type Static, Type } from "typebox";
import { type Component, Text, truncateToWidth } from "@earendil-works/pi-tui";

import { AutoskCliError, runAutoskJson, type RunOptions } from "./cli.ts";
import type { AutoskDetails, Comment, CommentAction } from "./types.ts";

type ToolTheme = ExtensionContext["ui"]["theme"];

const DOMAIN = "comment" as const;
const TEXT_TRUNCATE = 120;

// ---------------------------------------------------------------------------
// schema
// ---------------------------------------------------------------------------

const CommentActionSchema = Type.Union([Type.Literal("add"), Type.Literal("list")]);

const CommentArgsSchema = Type.Partial(
	Type.Object({
		/** Target task id. Always required. */
		task_id: Type.String({ description: "Target task id (e.g. as-a1b2). Required for add and list." }),
		/** Comment text. add only. */
		text: Type.String({ description: "Comment text. Required by add; non-empty." }),
		/** Optional author override. Defaults to $AUTOSK_AGENT or 'human'. */
		author: Type.String({
			description: "Optional author name override. Defaults to the AUTOSK_AGENT env value (or 'human').",
		}),
	}),
);

export const CommentParamsSchema = Type.Object({
	action: CommentActionSchema,
	args: Type.Optional(CommentArgsSchema),
});

export type CommentArgs = Static<typeof CommentArgsSchema>;
export type CommentParams = Static<typeof CommentParamsSchema>;

// ---------------------------------------------------------------------------
// tool description
// ---------------------------------------------------------------------------

const TOOL_DESCRIPTION = `Append-only comments on autosk tasks. The workflow engine surfaces every prior comment at the top of each step's prompt, so this is the canonical cross-agent channel for a task.

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

Comments are immutable. Don't try to "edit" — append a new comment instead.`;

const TOOL_PROMPT_SNIPPET =
	"Append / read durable comments on autosk tasks via autosk_comment.";

const TOOL_PROMPT_GUIDELINES = [
	"Use `autosk_comment` to record progress notes, hand-offs, and follow-ups on a task; do not paste them into chat instead.",
	"Comments on an autosk task are immutable. To correct a previous `autosk_comment` add, append a new comment, never try to edit.",
] as const;

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

export function registerCommentTool(pi: ExtensionAPI) {
	pi.registerTool({
		name: "autosk_comment",
		label: "autosk comment",
		description: TOOL_DESCRIPTION,
		promptSnippet: TOOL_PROMPT_SNIPPET,
		promptGuidelines: [...TOOL_PROMPT_GUIDELINES],
		parameters: CommentParamsSchema,
		execute: (_callId, params, signal, _onUpdate, ctx) =>
			executeCommentTool(pi, params as CommentParams, signal, ctx),
		renderCall: renderCommentCall,
		renderResult: renderCommentResult,
	});
}

async function executeCommentTool(
	pi: Pick<ExtensionAPI, "exec">,
	params: CommentParams,
	signal: AbortSignal | undefined,
	ctx: ExtensionContext,
) {
	const args: CommentArgs = params.args ?? {};
	const runOptions: RunOptions = { cwd: ctx.cwd, signal };
	const action = params.action;
	try {
		if (action === "add") {
			const comment = await runAdd(pi, args, runOptions);
			const details: AutoskDetails = { kind: "comment", domain: DOMAIN, action, comment };
			return {
				content: [{ type: "text" as const, text: formatAddText(comment) }],
				details,
				isError: false,
			};
		}
		const { task_id, comments } = await runList(pi, args, runOptions);
		const details: AutoskDetails = {
			kind: "comments",
			domain: DOMAIN,
			action: "list",
			task_id,
			comments,
		};
		return {
			content: [{ type: "text" as const, text: formatListText(task_id, comments) }],
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

async function runAdd(
	pi: Pick<ExtensionAPI, "exec">,
	args: CommentArgs,
	runOptions: RunOptions,
): Promise<Comment> {
	const taskId = requireTaskId("add", args);
	const text = (args.text ?? "").trim();
	if (text === "") {
		throw new AutoskCliError(DOMAIN, "add", "invalid_args", "add requires non-empty `text`");
	}
	const argv: string[] = ["comment", "add", taskId, args.text ?? ""];
	if (args.author) argv.push("--author", args.author);
	return runAutoskJson<Comment>(pi, DOMAIN, "add", argv, runOptions);
}

async function runList(
	pi: Pick<ExtensionAPI, "exec">,
	args: CommentArgs,
	runOptions: RunOptions,
): Promise<{ task_id: string; comments: Comment[] }> {
	const taskId = requireTaskId("list", args);
	const comments = await runAutoskJson<Comment[]>(
		pi,
		DOMAIN,
		"list",
		["comment", "list", taskId],
		runOptions,
	);
	return { task_id: taskId, comments };
}

function requireTaskId(action: CommentAction, args: CommentArgs): string {
	const id = (args.task_id ?? "").trim();
	if (id === "") {
		throw new AutoskCliError(DOMAIN, action, "invalid_args", `${action} requires \`task_id\``);
	}
	return id;
}

// ---------------------------------------------------------------------------
// LLM-facing text
// ---------------------------------------------------------------------------

function formatAddText(c: Comment): string {
	return `added comment id=${c.id} on ${c.task_id} by ${c.author}\n${JSON.stringify(c)}`;
}

function formatListText(taskId: string, comments: Comment[]): string {
	if (comments.length === 0) {
		return `${taskId}: no comments\n[]`;
	}
	const lines = comments.map(
		(c) => `[${c.id}] ${c.author} @ ${c.created_at}: ${oneLine(c.text)}`,
	);
	return `${taskId}: ${comments.length} comment(s)\n${lines.join("\n")}\n${JSON.stringify(comments)}`;
}

function oneLine(text: string): string {
	return text.replace(/\s+/g, " ").slice(0, 200);
}

function normalizeError(
	action: CommentAction,
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
// renderers
// ---------------------------------------------------------------------------

function renderCommentCall(args: unknown, theme: ToolTheme): Component {
	const params = (args ?? {}) as CommentParams;
	const action = (params.action ?? "?") as CommentAction | "?";
	const summary = summarizeArgs(action, params.args ?? {});
	let line = theme.fg("toolTitle", theme.bold(`autosk_comment ${action}`));
	if (summary) line += " " + theme.fg("dim", truncateToWidth(summary, 80));
	return new Text(line, 0, 0);
}

function summarizeArgs(action: CommentAction | "?", args: CommentArgs): string {
	switch (action) {
		case "add": {
			if (!args.task_id) return "";
			const snippet = args.text ? `"${oneLine(args.text).slice(0, 40)}…"` : "";
			const author = args.author ? ` by ${args.author}` : "";
			return `${args.task_id} ${snippet}${author}`.trim();
		}
		case "list":
			return args.task_id ?? "";
		default:
			return "";
	}
}

interface ToolResult {
	content: Array<{ type?: string; text?: string }>;
	details?: AutoskDetails;
}

function renderCommentResult(
	result: ToolResult,
	_options: unknown,
	theme: ToolTheme,
): Component {
	const details = result.details;
	if (!details) return new Text(fallbackText(result), 0, 0);
	if (details.kind === "error") return new Text(renderError(details, theme), 0, 0);
	if (details.kind === "comment") {
		return new Text(renderSingle(details.comment, theme), 0, 0);
	}
	if (details.kind === "comments") {
		return new Text(renderList(details.task_id, details.comments, theme), 0, 0);
	}
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

function renderSingle(c: Comment, theme: ToolTheme): string {
	const head = `${theme.bold(theme.fg("accent", `[${c.id}]`))} ${theme.fg("muted", c.author)} ${theme.fg("dim", c.created_at)} ${theme.fg("muted", `→ ${c.task_id}`)}`;
	const body = theme.fg("text", truncateToWidth(c.text.replace(/\n/g, " "), TEXT_TRUNCATE));
	return `${head}\n${body}`;
}

function renderList(taskId: string, comments: Comment[], theme: ToolTheme): string {
	if (comments.length === 0) {
		return theme.fg("muted", `${taskId}: no comments`);
	}
	const lines: string[] = [theme.fg("dim", `${taskId}: ${comments.length} comment(s)`)];
	for (const c of comments) {
		const id = theme.fg("accent", `[${c.id}]`);
		const who = theme.fg("muted", c.author);
		const when = theme.fg("dim", c.created_at);
		const text = truncateToWidth(c.text.replace(/\n/g, " "), TEXT_TRUNCATE);
		lines.push(`${id} ${who} ${when}  ${text}`);
	}
	return lines.join("\n");
}

function fallbackText(result: ToolResult): string {
	const first = result.content[0];
	if (first && first.type === "text" && typeof first.text === "string") return first.text;
	return "";
}
