/**
 * `autosk` CLI shell-out + `--json` parsing for the `autoskd mcp` server's
 * `task` / `comment` tools. Ported from `@autosk/pi-tools` `src/cli.ts`, with the
 * pi-runtime `pi.exec` swapped for a self-contained {@link runProcess} built on
 * `Bun.spawn` (the compiled daemon embeds the Bun runtime, so this needs no
 * global node/bun).
 *
 * The `autosk` CLI is the single place that already centralizes the env nuances
 * we must not re-implement — the `AUTOSK_CWD` project selector, `AUTOSK_SOCK`
 * socket resolution, daemon connect-or-auto-spawn, the RFC3339/JSON wire format,
 * and `AUTOSK_AGENT` author attribution. `autoskd mcp` inherits those env vars
 * (baked into the `--mcp-config` by `@autosk/claude-agent`) and forwards them to
 * the spawned `autosk`, so the CLI joins the running daemon and targets the
 * right project even when Claude runs inside an isolated worktree.
 *
 * `autosk` (the Go CLI) must be resolvable on `PATH` — or via `$AUTOSK_BIN` — at
 * runtime, the same requirement `@autosk/pi-tools` already imposes.
 */

import type { AutoskDomain, AutoskErrorReason } from "./types.ts";

/** The `autosk` binary to spawn: `$AUTOSK_BIN` override, else `"autosk"` on PATH. */
export function autoskBin(): string {
  const bin = process.env.AUTOSK_BIN;
  return bin && bin.length > 0 ? bin : "autosk";
}

const DEFAULT_TIMEOUT_MS = 30_000;

export interface RunResult {
  stdout: string;
  stderr: string;
  code: number;
  killed: boolean;
}

export interface RunOptions {
  signal?: AbortSignal;
  timeoutMs?: number;
}

/** A one-shot child runner — overridable in tests; defaults to {@link bunRunProcess}. */
export type RunProcess = (bin: string, argv: string[], options: RunOptions) => Promise<RunResult>;

/**
 * Error thrown by the cli helpers. Always carries enough context for the caller
 * to wrap it into a stable `AutoskDetails` record (domain, action, reason,
 * optional stdio).
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
 * Runs the `autosk` binary once to completion via `Bun.spawn`, capturing
 * stdout/stderr. A spawn ENOENT (binary not on PATH) is surfaced as an empty
 * `{ code:1, stdout:"", stderr:"" }` — the same shape pi's `pi.exec` produces —
 * so {@link looksLikeMissingBinary} can map it to a clear `missing_binary` error.
 */
export async function bunRunProcess(bin: string, argv: string[], options: RunOptions): Promise<RunResult> {
  let proc: ReturnType<typeof Bun.spawn>;
  try {
    proc = Bun.spawn([bin, ...argv], {
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
      env: process.env as Record<string, string>,
    });
  } catch {
    // ENOENT (binary not found) and other spawn failures collapse to the
    // empty-result "missing binary" heuristic shape.
    return { stdout: "", stderr: "", code: 1, killed: false };
  }

  let killed = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  if (options.timeoutMs ?? DEFAULT_TIMEOUT_MS) {
    timer = setTimeout(() => {
      killed = true;
      proc.kill();
    }, options.timeoutMs ?? DEFAULT_TIMEOUT_MS);
  }
  const onAbort = (): void => {
    killed = true;
    proc.kill();
  };
  if (options.signal) {
    if (options.signal.aborted) onAbort();
    else options.signal.addEventListener("abort", onAbort, { once: true });
  }

  try {
    const [stdout, stderr] = await Promise.all([
      new Response(proc.stdout as ReadableStream<Uint8Array>).text(),
      new Response(proc.stderr as ReadableStream<Uint8Array>).text(),
    ]);
    const code = await proc.exited;
    return { stdout, stderr, code: code ?? 0, killed };
  } finally {
    if (timer !== undefined) clearTimeout(timer);
    if (options.signal) options.signal.removeEventListener("abort", onAbort);
  }
}

/**
 * Spawn the local `autosk` binary with the given argv, mapping process failures
 * onto a stable {@link AutoskCliError}. Ported from `@autosk/pi-tools`'
 * `runAutosk`.
 */
export async function runAutosk(
  run: RunProcess,
  domain: AutoskDomain,
  action: string,
  argv: string[],
  options: RunOptions = {},
): Promise<RunResult> {
  let result: RunResult;
  try {
    result = await run(autoskBin(), argv, options);
  } catch (err) {
    if (err instanceof Error && err.name === "AbortError") {
      throw new AutoskCliError(domain, action, "aborted", "autosk execution was aborted");
    }
    const message = err instanceof Error ? err.message : String(err);
    throw new AutoskCliError(domain, action, "cli_error", `autosk exec failed: ${message}`);
  }

  if (result.killed) {
    throw new AutoskCliError(domain, action, "aborted", "autosk execution timed out or was aborted", {
      stdout: result.stdout,
      stderr: result.stderr,
      code: result.code,
    });
  }
  if (result.code !== 0) {
    if (looksLikeMissingBinary(result)) {
      throw new AutoskCliError(
        domain,
        action,
        "missing_binary",
        "`autosk` binary not found on PATH. Build it (`make build`) and put ./bin on PATH, or symlink ./bin/autosk into /usr/local/bin (or set $AUTOSK_BIN).",
        { stdout: result.stdout, stderr: result.stderr, code: result.code },
      );
    }
    const detail = result.stderr.trim() || result.stdout.trim() || `(no output, exit ${result.code})`;
    throw new AutoskCliError(domain, action, "cli_error", `autosk ${action} failed: ${detail}`, {
      stdout: result.stdout,
      stderr: result.stderr,
      code: result.code,
    });
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
    throw new AutoskCliError(domain, action, "parse_error", `failed to parse autosk JSON: ${message}`, {
      stdout: raw,
    });
  }
}

/** Run with `--json` appended, then parse stdout as `T`. */
export async function runAutoskJson<T>(
  run: RunProcess,
  domain: AutoskDomain,
  action: string,
  argv: string[],
  options: RunOptions = {},
): Promise<T> {
  const result = await runAutosk(run, domain, action, [...argv, "--json"], options);
  return parseJson<T>(domain, action, result.stdout);
}

function looksLikeMissingBinary(result: RunResult): boolean {
  if (result.killed || result.code === 0) return false;
  return result.stdout.trim().length === 0 && result.stderr.trim().length === 0;
}
