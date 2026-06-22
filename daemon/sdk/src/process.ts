/**
 * Generic child-process primitives — the `Bun.spawn` stdio/abort plumbing behind
 * `ctx.exec` (one-shot) and `ctx.spawn` (long-lived, stdio-streamed), extracted
 * from the daemon's `engine/child.ts` so BOTH the engine and a userspace sandbox
 * library (e.g. `@autosk/sandbox`, whose `dockerSandbox.wrap` rewrites an argv to
 * `docker run …`) can import ONE implementation instead of reimplementing
 * `LineDispatcher` / abort wiring.
 *
 * These are intentionally pi-free and engine-free: no "defaults" injection, no
 * env-merge policy beyond inheriting `process.env`. The caller passes a fully
 * resolved `signal` (and optional `cwd`/`env`); the engine layers its
 * `ctx.cwd` / `ctx.signal` defaults on top in `engine/child.ts`, and a sandbox's
 * `wrap` rewrites the argv (e.g. `docker run …`) before the agent calls here.
 *
 * Runtime-only (Bun): never a wire type, so the Go/Tauri proto mirror is
 * untouched. `@autosk/sdk` already ships runtime helpers (`statusStep`, ids) and
 * targets Bun, so a `Bun.spawn`-based module is in keeping with the package.
 */

import type { ChildHandle, ExecResult } from "./agent.ts";

/** Options for {@link runChild}: a fully resolved one-shot child invocation. */
export interface RunChildOptions {
  /** Working directory. When omitted, Bun inherits the parent process cwd. */
  cwd?: string;
  /** Extra env, merged OVER `process.env` (so PATH/HOME survive). */
  env?: Record<string, string>;
  /** Data written to the child's stdin, then closed. */
  input?: string | Uint8Array;
  /** Aborts the child (kill). Required — the caller resolves the default. */
  signal: AbortSignal;
  /** Kill the child after this many milliseconds. */
  timeoutMs?: number;
}

/** Options for {@link spawnChild}: a fully resolved long-lived child invocation. */
export interface SpawnChildOptions {
  /** Working directory. When omitted, Bun inherits the parent process cwd. */
  cwd?: string;
  /** Extra env, merged OVER `process.env` (so PATH/HOME survive). */
  env?: Record<string, string>;
  /** Aborts the child (kill). Required — the caller resolves the default. */
  signal: AbortSignal;
}

function toBytes(input: string | Uint8Array): Uint8Array {
  return typeof input === "string" ? new TextEncoder().encode(input) : input;
}

function mergedEnv(env?: Record<string, string>): Record<string, string> | undefined {
  if (!env) return undefined;
  return { ...(process.env as Record<string, string>), ...env };
}

/** Runs a one-shot child to completion, capturing stdout/stderr (plan §4.2). */
export async function runChild(cmd: string[], opts: RunChildOptions): Promise<ExecResult> {
  const { signal } = opts;
  const proc = Bun.spawn(cmd, {
    cwd: opts.cwd,
    env: mergedEnv(opts.env),
    stdin: opts.input !== undefined ? toBytes(opts.input) : "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });

  let timer: ReturnType<typeof setTimeout> | undefined;
  // A timeout means "this command is hung — force-kill it", so we SIGKILL (9)
  // rather than SIGTERM. Beyond being the right semantic for a deadline, SIGKILL
  // guarantees the non-zero ExecResult the contract promises even for children
  // that SWALLOW SIGTERM and exit 0 — notably a `docker` CLI client a sandbox's
  // `wrap` rewrites the argv to (e.g. `docker run …`), which can trap SIGTERM and
  // return 0, so a graceful kill there would mask the timeout. The abort path
  // below stays SIGTERM (cooperative cancellation, not a hung-command kill).
  if (opts.timeoutMs !== undefined) timer = setTimeout(() => proc.kill(9), opts.timeoutMs);
  const onAbort = (): void => proc.kill();
  if (signal.aborted) proc.kill();
  else signal.addEventListener("abort", onAbort, { once: true });

  try {
    const [stdout, stderr] = await Promise.all([
      new Response(proc.stdout).text(),
      new Response(proc.stderr).text(),
    ]);
    const code = await proc.exited;
    return { code, stdout, stderr };
  } finally {
    if (timer !== undefined) clearTimeout(timer);
    signal.removeEventListener("abort", onAbort);
  }
}

/** Spawns a long-lived child with line-buffered stdout/stderr (plan §4.2). */
export function spawnChild(cmd: string[], opts: SpawnChildOptions): ChildHandle {
  const proc = Bun.spawn(cmd, {
    cwd: opts.cwd,
    env: mergedEnv(opts.env),
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  });

  const stdinStream = new WritableStream<Uint8Array>({
    write(chunk) {
      proc.stdin.write(chunk);
    },
    close() {
      proc.stdin.end();
    },
    abort() {
      proc.stdin.end();
    },
  });
  const stdin = stdinStream.getWriter();

  const stdout = new LineDispatcher();
  const stderr = new LineDispatcher();
  void readLines(proc.stdout as ReadableStream<Uint8Array>, (line) => stdout.emit(line));
  void readLines(proc.stderr as ReadableStream<Uint8Array>, (line) => stderr.emit(line));

  const onAbort = (): void => proc.kill();
  if (opts.signal.aborted) proc.kill();
  else opts.signal.addEventListener("abort", onAbort, { once: true });

  const exited = proc.exited.then((code) => {
    opts.signal.removeEventListener("abort", onAbort);
    return { code };
  });

  return {
    stdin,
    onStdout: (cb) => stdout.subscribe(cb),
    onStderr: (cb) => stderr.subscribe(cb),
    kill: (signal) => proc.kill(signal as number | NodeJS.Signals | undefined),
    exited,
  };
}

/**
 * Fans line-buffered output to callbacks. Lines that arrive before the FIRST
 * subscriber are buffered and replayed to it, so a caller that registers
 * `onStdout` a tick after `spawn()` never misses early output.
 *
 * NOTE: the replay buffer is drained by the first subscriber only — a second
 * `onStdout`/`onStderr` registered later sees only lines that arrive AFTER it
 * subscribes (early output is already consumed). That suffices for the
 * single-reader pi-agent (P6); a multi-reader caller would need per-subscriber
 * buffering.
 */
class LineDispatcher {
  private readonly cbs: ((line: string) => void)[] = [];
  private buffer: string[] = [];

  subscribe(cb: (line: string) => void): void {
    this.cbs.push(cb);
    if (this.buffer.length > 0) {
      const pending = this.buffer;
      this.buffer = [];
      for (const line of pending) cb(line);
    }
  }

  emit(line: string): void {
    if (this.cbs.length === 0) {
      this.buffer.push(line);
      return;
    }
    for (const cb of this.cbs) cb(line);
  }
}

/** Reads a byte stream and emits complete (`\n`-delimited, `\r`-trimmed) lines. */
async function readLines(
  stream: ReadableStream<Uint8Array>,
  onLine: (line: string) => void,
): Promise<void> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx: number;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        onLine(line.endsWith("\r") ? line.slice(0, -1) : line);
      }
    }
    buf += decoder.decode();
    if (buf.length > 0) onLine(buf.endsWith("\r") ? buf.slice(0, -1) : buf);
  } catch {
    /* stream aborted/closed underneath us — nothing more to read */
  }
}
