/**
 * Child-process primitives backing `ctx.exec` (one-shot) and `ctx.spawn`
 * (long-lived, stdio-streamed) — plan §3.4.
 *
 * Core has no pi knowledge: these are generic process helpers the `@autosk/pi-agent`
 * extension (P6) builds on to drive `pi --mode rpc` over JSON-lines stdio. Both
 * default the child's cwd to the session's `ctx.cwd` (project root or isolation
 * path) and wire the session's `AbortSignal` so an abort / daemon shutdown kills
 * the child.
 */

import type { ChildHandle, ExecOptions, ExecResult, SpawnOptions } from "@autosk/sdk";

function toBytes(input: string | Uint8Array): Uint8Array {
  return typeof input === "string" ? new TextEncoder().encode(input) : input;
}

function mergedEnv(env?: Record<string, string>): Record<string, string> | undefined {
  if (!env) return undefined;
  return { ...(process.env as Record<string, string>), ...env };
}

/** Defaults the engine injects into `ctx.exec`. */
export interface ExecDefaults {
  defaultCwd: string;
  defaultSignal: AbortSignal;
}

/** Runs a one-shot child to completion, capturing stdout/stderr (plan §3.4). */
export async function execOneShot(
  cmd: string[],
  opts: ExecOptions & ExecDefaults,
): Promise<ExecResult> {
  const signal = opts.signal ?? opts.defaultSignal;
  const proc = Bun.spawn(cmd, {
    cwd: opts.cwd ?? opts.defaultCwd,
    env: mergedEnv(opts.env),
    stdin: opts.input !== undefined ? toBytes(opts.input) : "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });

  let timer: ReturnType<typeof setTimeout> | undefined;
  if (opts.timeoutMs !== undefined) timer = setTimeout(() => proc.kill(), opts.timeoutMs);
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

/** Defaults the engine injects into `ctx.spawn`. */
export interface SpawnDefaults {
  defaultCwd: string;
  signal: AbortSignal;
}

/** Spawns a long-lived child with line-buffered stdout/stderr (plan §3.4). */
export function spawnChild(cmd: string[], opts: SpawnOptions & SpawnDefaults): ChildHandle {
  const proc = Bun.spawn(cmd, {
    cwd: opts.cwd ?? opts.defaultCwd,
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
