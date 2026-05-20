/**
 * Lightweight smoke test: spins up a throwaway .autosk/db and exercises the
 * three tool execute() handlers (task / comment / step) end-to-end against a
 * real `autosk` binary.
 *
 * Skips the `autosk_step next` happy path because it requires a running
 * workflow daemon. The negative path (no active run → cli_error) is covered
 * here so we still get coverage of the step tool's error wiring.
 *
 * Run with: node --experimental-strip-types test/smoke.ts
 */
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { registerCommentTool } from "../src/comment.ts";
import { registerStepTool } from "../src/step.ts";
import { registerTaskTool } from "../src/task.ts";

const cwd = mkdtempSync(join(tmpdir(), "autosk-ext-smoke-"));
process.on("exit", () => {
	try {
		rmSync(cwd, { recursive: true, force: true });
	} catch {
		// best-effort cleanup
	}
});

interface ExecOptions {
	cwd?: string;
	signal?: AbortSignal;
	timeout?: number;
}
interface ExecResult {
	stdout: string;
	stderr: string;
	code: number;
	killed: boolean;
}

function exec(command: string, args: string[], options?: ExecOptions): Promise<ExecResult> {
	return new Promise((resolve) => {
		const proc = spawn(command, args, { cwd: options?.cwd, shell: false });
		let stdout = "";
		let stderr = "";
		proc.stdout?.on("data", (b) => {
			stdout += b.toString();
		});
		proc.stderr?.on("data", (b) => {
			stderr += b.toString();
		});
		proc.on("error", () => resolve({ stdout, stderr, code: 1, killed: false }));
		proc.on("close", (code) => resolve({ stdout, stderr, code: code ?? 0, killed: false }));
	});
}

// Minimal stub: collect tool registrations and dispatch to their execute().
interface RegisteredTool {
	name: string;
	execute: (
		callId: string,
		params: unknown,
		signal: AbortSignal | undefined,
		onUpdate: undefined,
		ctx: { cwd: string },
	) => Promise<{ details?: { kind: string; [k: string]: unknown }; isError?: boolean }>;
}

const tools = new Map<string, RegisteredTool>();
const pi = {
	exec,
	registerTool(def: RegisteredTool) {
		tools.set(def.name, def);
	},
	// methods we don't use, but keep present so type checkers won't whine
	// (extension code doesn't call them anyway).
} as unknown as Parameters<typeof registerTaskTool>[0];

registerTaskTool(pi);
registerCommentTool(pi);
registerStepTool(pi);

const failures: string[] = [];
const ctx = { cwd } as any;

async function call(
	label: string,
	toolName: string,
	params: unknown,
	expect: "ok" | "error" = "ok",
) {
	const tool = tools.get(toolName);
	if (!tool) throw new Error(`tool ${toolName} not registered`);
	const r = await tool.execute("call-1", params, undefined, undefined, ctx);
	const kind = r.details?.kind ?? "?";
	const gotError = kind === "error";
	const matched = expect === "ok" ? !gotError : gotError;
	console.log(`${matched ? "ok  " : "FAIL"} ${label}  [${kind}]`);
	if (!matched) {
		const msg = gotError
			? (r.details as { message: string }).message
			: `expected error, got ${kind}`;
		failures.push(`${label}: ${msg}`);
	}
	return r.details as any;
}

// --- task ----------------------------------------------------------------

const parent = await call("task.create parent", "autosk_task", {
	action: "create",
	args: { title: "parent task", priority: 1 },
});
const parentId = parent.task.id as string;

const child = await call("task.create child blocks parent", "autosk_task", {
	action: "create",
	args: { title: "child task", blocks: [parentId] },
});
const childId = child.task.id as string;

await call("task.show parent", "autosk_task", { action: "show", args: { id: parentId } });

await call("task.update child p=0", "autosk_task", {
	action: "update",
	args: { id: childId, priority: 0, description: "updated by smoke" },
});

// negative — create without title
await call(
	"task.create without title (→ invalid_args)",
	"autosk_task",
	{ action: "create", args: {} },
	"error",
);

// negative — update with no fields
await call(
	"task.update with no fields (→ invalid_args)",
	"autosk_task",
	{ action: "update", args: { id: childId } },
	"error",
);

// negative — show non-existent id
await call(
	"task.show as-zzzz (→ cli_error)",
	"autosk_task",
	{ action: "show", args: { id: "as-zzzz" } },
	"error",
);

// --- comment -------------------------------------------------------------

await call("comment.add", "autosk_comment", {
	action: "add",
	args: { task_id: parentId, text: "first comment from smoke" },
});

await call("comment.add (with author)", "autosk_comment", {
	action: "add",
	args: { task_id: parentId, text: "follow-up", author: "smoke-tester" },
});

const list = await call("comment.list", "autosk_comment", {
	action: "list",
	args: { task_id: parentId },
});
if (list.comments.length !== 2) {
	failures.push(`expected 2 comments, got ${list.comments.length}`);
}

// negative — add without text
await call(
	"comment.add without text (→ invalid_args)",
	"autosk_comment",
	{ action: "add", args: { task_id: parentId } },
	"error",
);

// --- step ----------------------------------------------------------------

// No daemon, no workflow → CLI rejects "no active run". We just want to confirm
// the tool wires the error correctly.
await call(
	"step.next without active run (→ cli_error)",
	"autosk_step",
	{ action: "next", args: { task_id: parentId, to: "done" } },
	"error",
);

await call(
	"step.next missing `to` (→ invalid_args)",
	"autosk_step",
	{ action: "next", args: { task_id: parentId } },
	"error",
);

if (failures.length > 0) {
	console.error("\nFAILURES:");
	for (const f of failures) console.error("  - " + f);
	process.exit(1);
}
console.log("\nall green");
