import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import type { AutoskAction, AutoskErrorReason, Task } from "./types.ts";

const AUTOSK_BIN = "autosk";

export interface RunResult {
	stdout: string;
	stderr: string;
	code: number;
	killed: boolean;
}

export interface RunOptions {
	cwd: string;
	signal?: AbortSignal;
	timeoutMs?: number;
}

export class AutoskCliError extends Error {
	readonly action: AutoskAction;
	readonly reason: AutoskErrorReason;
	readonly stdout: string;
	readonly stderr: string;
	readonly code: number;

	constructor(
		action: AutoskAction,
		reason: AutoskErrorReason,
		message: string,
		opts: { stdout?: string; stderr?: string; code?: number } = {},
	) {
		super(message);
		this.action = action;
		this.reason = reason;
		this.stdout = opts.stdout ?? "";
		this.stderr = opts.stderr ?? "";
		this.code = opts.code ?? 0;
	}
}

/**
 * Spawn `autosk` with the given argv. Any non-zero exit becomes an
 * `AutoskCliError`.
 *
 * `pi.exec` swallows spawn errors and returns `code=1` with empty
 * stdout/stderr, which collides with a real autosk failure that happens to
 * produce no stderr. We use that empty-result shape as the missing-binary
 * heuristic; everything else is treated as a normal CLI failure.
 */
export async function runAutosk(
	pi: Pick<ExtensionAPI, "exec">,
	action: AutoskAction,
	argv: string[],
	options: RunOptions,
): Promise<RunResult> {
	let result: RunResult;
	try {
		result = await pi.exec(AUTOSK_BIN, argv, {
			cwd: options.cwd,
			signal: options.signal,
			timeout: options.timeoutMs ?? 30_000,
		});
	} catch (err) {
		if (err instanceof Error && err.name === "AbortError") {
			throw new AutoskCliError(action, "aborted", "autosk execution was aborted");
		}
		const message = err instanceof Error ? err.message : String(err);
		throw new AutoskCliError(action, "cli_error", `autosk exec failed: ${message}`);
	}

	if (result.killed) {
		throw new AutoskCliError(action, "aborted", "autosk execution timed out or was aborted", {
			stdout: result.stdout,
			stderr: result.stderr,
			code: result.code,
		});
	}
	if (result.code !== 0) {
		if (looksLikeMissingBinary(result)) {
			throw new AutoskCliError(
				action,
				"missing_binary",
				"`autosk` binary not found on PATH. Build it from ~/me/dev/autosk (`make build`) and put ./bin on PATH, or symlink ./bin/autosk into /usr/local/bin.",
				{ stdout: result.stdout, stderr: result.stderr, code: result.code },
			);
		}
		const trimmedStderr = result.stderr.trim();
		const trimmedStdout = result.stdout.trim();
		const detail = trimmedStderr || trimmedStdout || `(no output, exit ${result.code})`;
		throw new AutoskCliError(action, "cli_error", `autosk ${action} failed: ${detail}`, {
			stdout: result.stdout,
			stderr: result.stderr,
			code: result.code,
		});
	}
	return result;
}

/** Parse a JSON payload from autosk; raises an AutoskCliError on garbage. */
export function parseJson<T>(action: AutoskAction, raw: string): T {
	const trimmed = raw.trim();
	if (!trimmed) {
		throw new AutoskCliError(action, "parse_error", "autosk returned empty output where JSON was expected");
	}
	try {
		return JSON.parse(trimmed) as T;
	} catch (err) {
		const message = err instanceof Error ? err.message : String(err);
		throw new AutoskCliError(action, "parse_error", `failed to parse autosk JSON: ${message}`, {
			stdout: raw,
		});
	}
}

/**
 * Convenience: run autosk with `--json` and parse the result. Used by every
 * read command and every write command that supports --json natively.
 */
export async function runAutoskJson<T>(
	pi: Pick<ExtensionAPI, "exec">,
	action: AutoskAction,
	argv: string[],
	options: RunOptions,
): Promise<T> {
	const result = await runAutosk(pi, action, [...argv, "--json"], options);
	return parseJson<T>(action, result.stdout);
}

/** Refresh a task after a write that did not support `--json` (block/unblock). */
export async function fetchTask(
	pi: Pick<ExtensionAPI, "exec">,
	action: AutoskAction,
	id: string,
	options: RunOptions,
): Promise<Task> {
	return runAutoskJson<Task>(pi, action, ["show", id], options);
}

function looksLikeMissingBinary(result: RunResult): boolean {
	if (result.killed || result.code === 0) {
		return false;
	}
	return result.stdout.trim().length === 0 && result.stderr.trim().length === 0;
}
