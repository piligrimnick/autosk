import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { AutoskCliError, fetchTask, parseJson, runAutosk, runAutoskJson, type RunOptions } from "./autosk-cli.ts";
import type { AutoskArgs } from "./schema.ts";
import type { AutoskAction, AutoskDetails, Task } from "./types.ts";

interface ActionContext {
	pi: Pick<ExtensionAPI, "exec">;
	args: AutoskArgs;
	runOptions: RunOptions;
}

interface ActionResponse {
	text: string;
	details: AutoskDetails;
}

/** Top-level entry: dispatch to the per-action runner, normalize errors. */
export async function runAction(
	pi: Pick<ExtensionAPI, "exec">,
	action: AutoskAction,
	args: AutoskArgs,
	runOptions: RunOptions,
): Promise<ActionResponse> {
	const ctx: ActionContext = { pi, args, runOptions };
	try {
		switch (action) {
			case "create":
				return await runCreate(ctx);
			case "show":
				return await runShow(ctx);
			case "update":
				return await runUpdate(ctx);
			case "done":
			case "cancel":
			case "reopen":
				return await runStatusChange(action, ctx);
			case "step_next":
				return await runStepNext(ctx);
			case "block":
				return await runBlock(ctx);
			case "unblock":
				return await runUnblock(ctx);
			case "dep_list":
				return await runDepList(ctx);
			case "list":
				return await runList(ctx);
			case "ready":
				return await runReady(ctx);
			case "next":
				return await runNext(ctx);
			case "init":
				return await runInit(ctx);
		}
	} catch (err) {
		if (err instanceof AutoskCliError) {
			return {
				text: formatErrorText(err),
				details: {
					kind: "error",
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
}

// ---------------------------------------------------------------------------
// per-action runners
// ---------------------------------------------------------------------------

async function runCreate({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
	if (!args.title || args.title.trim() === "") {
		throw new AutoskCliError("create", "invalid_args", "create requires a non-empty `title`");
	}
	const argv: string[] = ["create", args.title];
	if (args.description !== undefined) {
		argv.push("--description", args.description);
	}
	if (args.priority !== undefined) {
		argv.push("--priority", String(args.priority));
	}
	for (const id of args.blocks ?? []) {
		argv.push("--blocks", id);
	}
	for (const id of args.blocked_by ?? []) {
		argv.push("--blocked-by", id);
	}
	const task = await runAutoskJson<Task>(pi, "create", argv, runOptions);
	return {
		text: `created ${task.id} (priority ${task.priority}, status ${task.status})\n${formatTaskLine(task)}\n${JSON.stringify(task)}`,
		details: { kind: "task", action: "create", task },
	};
}

async function runShow({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
	const id = requireId("show", args);
	const task = await runAutoskJson<Task>(pi, "show", ["show", id], runOptions);
	return {
		text: `${formatTaskLine(task)}\n${JSON.stringify(task)}`,
		details: { kind: "task", action: "show", task },
	};
}

async function runUpdate({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
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
			"update",
			"invalid_args",
			"update requires at least one of: title, description, priority, status",
		);
	}
	const task = await runAutoskJson<Task>(pi, "update", argv, runOptions);
	return {
		text: `updated ${task.id}\n${formatTaskLine(task)}\n${JSON.stringify(task)}`,
		details: { kind: "task", action: "update", task },
	};
}

async function runStatusChange(
	action: Extract<AutoskAction, "done" | "cancel" | "reopen">,
	{ pi, args, runOptions }: ActionContext,
): Promise<ActionResponse> {
	const id = requireId(action, args);
	const task = await runAutoskJson<Task>(pi, action, [action, id], runOptions);
	return {
		text: `${action} ${task.id} → status=${task.status}, blocked=${task.blocked}\n${JSON.stringify(task)}`,
		details: { kind: "task", action, task },
	};
}

/** runStepNext records a workflow transition signal (plan §5.4). */
async function runStepNext({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
	const id = requireId("step_next", args);
	if (!args.to || args.to.trim() === "") {
		throw new AutoskCliError(
			"step_next",
			"invalid_args",
			"step_next requires `to`: a sibling step name or one of done|cancelled|human_feedback",
		);
	}
	const result = await runAutosk(pi, "step_next", ["step", "next", id, "--to", args.to, "--json"], runOptions);
	const signal = parseJson<{
		run_id: string;
		task_id: string;
		transition_id: number;
		next_step?: string;
		task_status?: string;
		prompt_rule: string;
		created_at: string;
	}>("step_next", result.stdout);
	const target = signal.next_step ? `step=${signal.next_step}` : `task_status=${signal.task_status}`;
	const msg = `step_next ${signal.task_id}: recorded transition to ${target} (run=${signal.run_id})`;
	return {
		text: `${msg}\n${JSON.stringify(signal)}`,
		details: { kind: "ack", action: "step_next", message: msg },
	};
}

async function runBlock({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
	const id = requireId("block", args);
	const blockers = (args.blocker_ids ?? []).filter((s) => s.length > 0);
	if (blockers.length === 0) {
		throw new AutoskCliError("block", "invalid_args", "block requires `blocker_ids` (non-empty)");
	}
	// `block` does not honor --json yet; run it, then refresh state via show.
	await runAutosk(pi, "block", ["block", id, ...blockers], runOptions);
	const task = await fetchTask(pi, "block", id, runOptions);
	return {
		text: `blocked ${task.id} by [${blockers.join(", ")}]; blocked_by=[${task.blocked_by.join(", ")}], blocked=${task.blocked}\n${JSON.stringify(task)}`,
		details: { kind: "task", action: "block", task },
	};
}

async function runUnblock({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
	const id = requireId("unblock", args);
	const argv: string[] = ["unblock", id];
	if (args.all) {
		argv.push("--all");
	} else {
		const blockers = (args.blocker_ids ?? []).filter((s) => s.length > 0);
		if (blockers.length === 0) {
			throw new AutoskCliError(
				"unblock",
				"invalid_args",
				"unblock requires either `blocker_ids` (non-empty) or `all: true`",
			);
		}
		argv.push(...blockers);
	}
	await runAutosk(pi, "unblock", argv, runOptions);
	const task = await fetchTask(pi, "unblock", id, runOptions);
	return {
		text: `unblocked ${task.id}; blocked_by=[${task.blocked_by.join(", ")}], blocked=${task.blocked}\n${JSON.stringify(task)}`,
		details: { kind: "task", action: "unblock", task },
	};
}

async function runDepList({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
	const id = requireId("dep_list", args);
	// `dep list` has no JSON shape; use `show --json <id>` which already exposes blocked_by + blocks.
	const task = await fetchTask(pi, "dep_list", id, runOptions);
	return {
		text: `${task.id}: blocked_by=[${task.blocked_by.join(", ")}], blocks=[${task.blocks.join(", ")}], blocked=${task.blocked}\n${JSON.stringify(task)}`,
		details: { kind: "task", action: "dep_list", task },
	};
}

async function runList({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
	const argv: string[] = ["list"];
	if (args.status_filter !== undefined) {
		argv.push("--status", args.status_filter);
	}
	if (args.priority_filter !== undefined) {
		argv.push("--priority", String(args.priority_filter));
	}
	if (args.limit !== undefined) {
		argv.push("--limit", String(args.limit));
	}
	const tasks = await runAutoskJson<Task[]>(pi, "list", argv, runOptions);
	return {
		text: formatTaskListText("list", tasks),
		details: { kind: "tasks", action: "list", tasks },
	};
}

async function runReady({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
	const argv: string[] = ["ready"];
	if (args.limit !== undefined) {
		argv.push("--limit", String(args.limit));
	}
	const tasks = await runAutoskJson<Task[]>(pi, "ready", argv, runOptions);
	return {
		text: formatTaskListText("ready", tasks),
		details: { kind: "tasks", action: "ready", tasks },
	};
}

async function runNext({ pi, runOptions }: ActionContext): Promise<ActionResponse> {
	// `autosk next --json` is an outlier: it emits a single Task object (not an
	// array) on hit, and prints literal `null` with exit=1 when there is no ready
	// work. Both are normal outcomes for the agent, so we don't surface exit=1 as
	// an error here.
	let result;
	try {
		result = await runAutosk(pi, "next", ["next", "--json"], runOptions);
	} catch (err) {
		if (err instanceof AutoskCliError && err.reason === "cli_error" && err.stdout.trim() === "null") {
			return {
				text: formatTaskListText("next", []),
				details: { kind: "tasks", action: "next", tasks: [] },
			};
		}
		throw err;
	}
	const raw = result.stdout.trim();
	if (raw === "null" || raw === "") {
		return {
			text: formatTaskListText("next", []),
			details: { kind: "tasks", action: "next", tasks: [] },
		};
	}
	const task = parseJson<Task>("next", result.stdout);
	return {
		text: formatTaskListText("next", [task]),
		details: { kind: "tasks", action: "next", tasks: [task] },
	};
}

async function runInit({ pi, args, runOptions }: ActionContext): Promise<ActionResponse> {
	const argv: string[] = ["init"];
	if (args.prefix !== undefined) {
		argv.push("--prefix", args.prefix);
	}
	const result = await runAutosk(pi, "init", argv, runOptions);
	const message = result.stdout.trim() || "initialized .autosk/db";
	return {
		text: message,
		details: { kind: "ack", action: "init", message },
	};
}

// ---------------------------------------------------------------------------
// formatters used by the LLM-facing text (renderer formats its own visuals)
// ---------------------------------------------------------------------------

function formatTaskLine(t: Task): string {
	const blocked = t.blocked ? " blocked" : "";
	return `${t.id} p=${t.priority} ${t.status}${blocked} — ${t.title}`;
}

function formatTaskListText(action: AutoskAction, tasks: Task[]): string {
	if (tasks.length === 0) {
		return `${action}: no tasks\n[]`;
	}
	const lines = tasks.map(formatTaskLine);
	return `${action}: ${tasks.length} task(s)\n${lines.join("\n")}\n${JSON.stringify(tasks)}`;
}

function formatErrorText(err: AutoskCliError): string {
	return `[${err.reason}] ${err.message}`;
}

function requireId(action: AutoskAction, args: AutoskArgs): string {
	if (!args.id || args.id.trim() === "") {
		throw new AutoskCliError(action, "invalid_args", `${action} requires \`id\``);
	}
	return args.id;
}
