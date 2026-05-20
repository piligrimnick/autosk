import type {
	ExtensionAPI,
	ExtensionContext,
} from "@earendil-works/pi-coding-agent";
import { type Static, Type } from "typebox";
import { type Component, Text, truncateToWidth } from "@earendil-works/pi-tui";

import { AutoskCliError, runAutoskJson, type RunOptions } from "./cli.ts";
import type { AutoskDetails, StepAction, StepSignal } from "./types.ts";

type ToolTheme = ExtensionContext["ui"]["theme"];

const DOMAIN = "step" as const;

// ---------------------------------------------------------------------------
// schema
// ---------------------------------------------------------------------------

const StepActionSchema = Type.Union([Type.Literal("next")]);

const StepArgsSchema = Type.Partial(
	Type.Object({
		/** Target task id. */
		task_id: Type.String({
			description: "Task id whose active workflow run should be advanced. Required.",
		}),
		/** Transition target: sibling step name OR done|cancelled|human_feedback. */
		to: Type.String({
			description:
				"Transition target. Either a sibling step name in the current workflow, or one of {done, cancelled, human_feedback}. Required.",
		}),
	}),
);

export const StepParamsSchema = Type.Object({
	action: StepActionSchema,
	args: Type.Optional(StepArgsSchema),
});

export type StepArgs = Static<typeof StepArgsSchema>;
export type StepParams = Static<typeof StepParamsSchema>;

// ---------------------------------------------------------------------------
// tool description
// ---------------------------------------------------------------------------

const TOOL_DESCRIPTION = `Record the agent's chosen workflow transition for the active run on a task. Only call this while you are running inside a workflow step — it is the canonical way an agent "finishes" its step.

The autosk daemon resolves the task's active run, validates --to against the current step's outgoing transitions, and inserts a step_signal row; it then advances the task atomically after your turn ends.

Params: { action: "next", args: { task_id, to } }.

args.task_id (req)
  Task whose active workflow run should be advanced.

args.to (req)
  Transition target. Either:
    - a sibling step name in the current workflow (advance to that step), or
    - "done"           — close the task as completed,
    - "cancelled"      — close the task as cancelled,
    - "human_feedback" — pause the workflow waiting for a human.

Rules:
- step next may be called at most once per active run; the daemon rejects a second call.
- If there is no active run for the task (e.g. the task is in status='new'), the call fails with reason: "cli_error".

Result: details = { kind: "step_signal", domain: "step", action: "next", signal: {...} }
or      details = { kind: "error", domain: "step", action: "next", reason, message, ... }.`;

const TOOL_PROMPT_SNIPPET =
	"Record the workflow transition for the active task run via autosk_step.";

const TOOL_PROMPT_GUIDELINES = [
	"When you finish work inside a workflow step, call `autosk_step` with action:'next' to advance the task; never close in_workflow tasks via `autosk_task` update.",
	"`autosk_step` next can be called only once per run — if it already fired this run, do not call it again.",
] as const;

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

export function registerStepTool(pi: ExtensionAPI) {
	pi.registerTool({
		name: "autosk_step",
		label: "autosk step",
		description: TOOL_DESCRIPTION,
		promptSnippet: TOOL_PROMPT_SNIPPET,
		promptGuidelines: [...TOOL_PROMPT_GUIDELINES],
		parameters: StepParamsSchema,
		execute: (_callId, params, signal, _onUpdate, ctx) =>
			executeStepTool(pi, params as StepParams, signal, ctx),
		renderCall: renderStepCall,
		renderResult: renderStepResult,
	});
}

async function executeStepTool(
	pi: Pick<ExtensionAPI, "exec">,
	params: StepParams,
	signal: AbortSignal | undefined,
	ctx: ExtensionContext,
) {
	const args: StepArgs = params.args ?? {};
	const runOptions: RunOptions = { cwd: ctx.cwd, signal };
	const action = params.action;
	try {
		const signalOut = await runNext(pi, args, runOptions);
		const details: AutoskDetails = {
			kind: "step_signal",
			domain: DOMAIN,
			action,
			signal: signalOut,
		};
		return {
			content: [{ type: "text" as const, text: formatText(signalOut) }],
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
// action runner
// ---------------------------------------------------------------------------

async function runNext(
	pi: Pick<ExtensionAPI, "exec">,
	args: StepArgs,
	runOptions: RunOptions,
): Promise<StepSignal> {
	const taskId = (args.task_id ?? "").trim();
	if (taskId === "") {
		throw new AutoskCliError(DOMAIN, "next", "invalid_args", "next requires `task_id`");
	}
	const to = (args.to ?? "").trim();
	if (to === "") {
		throw new AutoskCliError(
			DOMAIN,
			"next",
			"invalid_args",
			"next requires `to` (sibling step name or one of done|cancelled|human_feedback)",
		);
	}
	return runAutoskJson<StepSignal>(
		pi,
		DOMAIN,
		"next",
		["step", "next", taskId, "--to", to],
		runOptions,
	);
}

function formatText(s: StepSignal): string {
	const target = s.next_step ? `step=${s.next_step}` : `task_status=${s.task_status ?? "?"}`;
	return `step_next ${s.task_id}: recorded transition to ${target} (run=${s.run_id})\n${JSON.stringify(s)}`;
}

function normalizeError(
	action: StepAction,
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

function renderStepCall(args: unknown, theme: ToolTheme): Component {
	const params = (args ?? {}) as StepParams;
	const action = (params.action ?? "?") as StepAction | "?";
	const a = params.args ?? {};
	const summary =
		a.task_id && a.to ? `${a.task_id} --to ${a.to}` : a.task_id || "";
	let line = theme.fg("toolTitle", theme.bold(`autosk_step ${action}`));
	if (summary) line += " " + theme.fg("dim", truncateToWidth(summary, 80));
	return new Text(line, 0, 0);
}

interface ToolResult {
	content: Array<{ type?: string; text?: string }>;
	details?: AutoskDetails;
}

function renderStepResult(
	result: ToolResult,
	_options: unknown,
	theme: ToolTheme,
): Component {
	const details = result.details;
	if (!details) return new Text(fallbackText(result), 0, 0);
	if (details.kind === "error") return new Text(renderError(details, theme), 0, 0);
	if (details.kind === "step_signal") {
		return new Text(renderSignal(details.signal, theme), 0, 0);
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

function renderSignal(s: StepSignal, theme: ToolTheme): string {
	const head = `${theme.bold(theme.fg("accent", s.task_id))}  ${theme.fg("muted", `run=${s.run_id}`)}`;
	const target = s.next_step
		? theme.fg("warning", `→ step=${s.next_step}`)
		: theme.fg("success", `→ status=${s.task_status ?? "?"}`);
	const rule = theme.fg("dim", `rule: ${s.prompt_rule}`);
	return `${head}\n${target}\n${rule}`;
}

function fallbackText(result: ToolResult): string {
	const first = result.content[0];
	if (first && first.type === "text" && typeof first.text === "string") return first.text;
	return "";
}
