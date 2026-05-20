import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import type { AutoskDomain, AutoskErrorReason } from "./types.ts";

const AUTOSK_BIN = "autosk";
const DEFAULT_TIMEOUT_MS = 30_000;

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

/**
 * Error thrown by the cli helpers. Always carries enough context for the
 * caller to wrap it into a stable `AutoskDetails` record (domain, action,
 * reason, optional stdio).
 */
export class AutoskCliError extends Error {
	readonly domain: AutoskDomain;
	readonly action: string;
	readonly reason: AutoskErrorReason;
	readonly stdout: string;
	readonly stderr: string;
	readonly code: number;

	constructor(
		domain: AutoskDomain,
		action: string,
		reason: AutoskErrorReason,
		message: string,
		opts: { stdout?: string; stderr?: string; code?: number } = {},
	) {
		super(message);
		this.domain = domain;
		this.action = action;
		this.reason = reason;
		this.stdout = opts.stdout ?? "";
		this.stderr = opts.stderr ?? "";
		this.code = opts.code ?? 0;
	}
}

/**
 * Spawn the local `autosk` binary with the given argv.
 *
 * `pi.exec` swallows ENOENT into `code=1 / stdout="" / stderr=""`, which
 * collides with a genuine autosk failure that happens to produce no
 * stderr. We use the empty-result shape as a "missing binary" heuristic,
 * matching the old extension's behaviour.
 */
export async function runAutosk(
	pi: Pick<ExtensionAPI, "exec">,
	domain: AutoskDomain,
	action: string,
	argv: string[],
	options: RunOptions,
): Promise<RunResult> {
	let result: RunResult;
	try {
		result = await pi.exec(AUTOSK_BIN, argv, {
			cwd: options.cwd,
			signal: options.signal,
			timeout: options.timeoutMs ?? DEFAULT_TIMEOUT_MS,
		});
	} catch (err) {
		if (err instanceof Error && err.name === "AbortError") {
			throw new AutoskCliError(domain, action, "aborted", "autosk execution was aborted");
		}
		const message = err instanceof Error ? err.message : String(err);
		throw new AutoskCliError(domain, action, "cli_error", `autosk exec failed: ${message}`);
	}

	if (result.killed) {
		throw new AutoskCliError(
			domain,
			action,
			"aborted",
			"autosk execution timed out or was aborted",
			{ stdout: result.stdout, stderr: result.stderr, code: result.code },
		);
	}
	if (result.code !== 0) {
		if (looksLikeMissingBinary(result)) {
			throw new AutoskCliError(
				domain,
				action,
				"missing_binary",
				"`autosk` binary not found on PATH. Build it from ~/me/dev/autosk (`make build`) and put ./bin on PATH, or symlink ./bin/autosk into /usr/local/bin.",
				{ stdout: result.stdout, stderr: result.stderr, code: result.code },
			);
		}
		const detail =
			result.stderr.trim() || result.stdout.trim() || `(no output, exit ${result.code})`;
		throw new AutoskCliError(
			domain,
			action,
			"cli_error",
			`autosk ${action} failed: ${detail}`,
			{ stdout: result.stdout, stderr: result.stderr, code: result.code },
		);
	}
	return result;
}

/** Parse JSON output from autosk; raises a parse_error on garbage. */
export function parseJson<T>(domain: AutoskDomain, action: string, raw: string): T {
	const trimmed = raw.trim();
	if (!trimmed) {
		throw new AutoskCliError(
			domain,
			action,
			"parse_error",
			"autosk returned empty output where JSON was expected",
		);
	}
	try {
		return JSON.parse(trimmed) as T;
	} catch (err) {
		const message = err instanceof Error ? err.message : String(err);
		throw new AutoskCliError(
			domain,
			action,
			"parse_error",
			`failed to parse autosk JSON: ${message}`,
			{ stdout: raw },
		);
	}
}

/** Run with `--json` appended, then parse stdout as `T`. */
export async function runAutoskJson<T>(
	pi: Pick<ExtensionAPI, "exec">,
	domain: AutoskDomain,
	action: string,
	argv: string[],
	options: RunOptions,
): Promise<T> {
	const result = await runAutosk(pi, domain, action, [...argv, "--json"], options);
	return parseJson<T>(domain, action, result.stdout);
}

function looksLikeMissingBinary(result: RunResult): boolean {
	if (result.killed || result.code === 0) return false;
	return result.stdout.trim().length === 0 && result.stderr.trim().length === 0;
}
