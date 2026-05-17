/**
 * Lightweight smoke test: invokes every action against a real autosk binary
 * inside a throwaway directory. Uses a hand-rolled `pi.exec` stub that calls
 * Node's child_process so we don't drag the pi runtime in.
 *
 * Run with: node --experimental-strip-types test/smoke.ts
 */
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runAction } from "../src/actions.ts";
import type { AutoskArgs } from "../src/schema.ts";
import type { AutoskAction, AutoskDetails } from "../src/types.ts";

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

const pi = {
	async exec(command: string, args: string[], options?: ExecOptions): Promise<ExecResult> {
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
	},
};

const failures: string[] = [];

async function run(
	label: string,
	action: AutoskAction,
	args: AutoskArgs,
	expect: "ok" | "error" = "ok",
): Promise<AutoskDetails> {
	const response = await runAction(pi, action, args, { cwd });
	const gotError = response.details.kind === "error";
	const matched = expect === "ok" ? !gotError : gotError;
	const prefix = matched ? "ok " : "FAIL";
	console.log(`${prefix} ${label}`);
	if (!matched) {
		const msg = gotError ? (response.details as { message: string }).message : `expected error, got ${response.details.kind}`;
		failures.push(`${label}: ${msg}`);
	}
	return response.details;
}

const created = await run("create parent", "create", { title: "parent", priority: 1 });
if (created.kind !== "task") throw new Error("create did not return a task");
const parentId = created.task.id;

const child1 = await run("create child1 (blocks parent)", "create", {
	title: "child1",
	blocks: [parentId],
});
const child2 = await run("create child2 (blocks parent)", "create", {
	title: "child2",
	blocks: [parentId],
});
if (child1.kind !== "task" || child2.kind !== "task") throw new Error("child create failed");

await run("ready", "ready", {});
await run("list (defaults)", "list", {});
await run("list (status=all)", "list", { status_filter: "all" });
await run("show parent", "show", { id: parentId });
await run("dep_list parent", "dep_list", { id: parentId });
await run("claim child1", "claim", { id: child1.task.id });
await run("update child1 priority", "update", { id: child1.task.id, priority: 0 });
await run("block parent (extra)", "block", {
	id: parentId,
	blocker_ids: [child1.task.id, child2.task.id],
});
await run("unblock --all parent", "unblock", { id: parentId, all: true });
await run("done child1", "done", { id: child1.task.id });
await run("cancel child2", "cancel", { id: child2.task.id });
await run("reopen child2", "reopen", { id: child2.task.id });
await run("next", "next", {});

// negative cases
const noId = await run("show without id (should error)", "show", {}, "error");
if (noId.kind === "error" && noId.reason !== "invalid_args") {
	failures.push(`expected invalid_args, got ${noId.reason}`);
}

const badId = await run("show as-zzzz (should error)", "show", { id: "as-zzzz" }, "error");
if (badId.kind === "error" && badId.reason !== "cli_error") {
	failures.push(`expected cli_error, got ${badId.reason}`);
}

if (failures.length > 0) {
	console.error("\nFAILURES:");
	for (const f of failures) console.error("  - " + f);
	process.exit(1);
}
console.log("\nall green");
