/**
 * Login-shell PATH enrichment — the "launched from a GUI app" fix.
 *
 * A daemon auto-spawned by a macOS `.app` bundle (or any launchd/Finder/GUI
 * context) inherits the minimal launchd PATH — `/usr/bin:/bin:/usr/sbin:/sbin`
 * — which omits Homebrew (`/opt/homebrew/bin`), nvm, asdf, pyenv, and friends.
 * Every child the daemon later spawns then breaks with "command not found":
 * `git worktree add` shells out to `git-lfs`, the first-run bootstrap shells out
 * to `npm`, the docker sandbox shells out to `docker`, and the agent steps shell
 * out to `pi` / `claude`.
 *
 * At startup the daemon asks the operator's login shell for its real PATH (the
 * same trick VS Code's `shell-env` / the `fix-path` package use) and merges it
 * into `process.env.PATH`, so every downstream spawn sees the user's full
 * toolchain regardless of how the daemon itself was launched.
 *
 * Opt out with `AUTOSK_SKIP_SHELL_PATH=1` (headless/air-gapped hosts that
 * already export a complete PATH, or to shave startup latency). The query is
 * also skipped automatically on Windows and under `bun test` (`NODE_ENV=test`).
 */

import { spawn } from "node:child_process";
import { delimiter as PATH_DELIM } from "node:path";

import type { Logger } from "../store/logger.ts";

/** Sentinel wrapping the PATH the login shell prints, so we can ignore rc noise. */
const MARKER = "__AUTOSK_PATH__";
/** Hard cap on the login-shell probe: a broken rc file must not wedge startup. */
const DEFAULT_TIMEOUT_MS = 2000;

export interface QueryOptions {
  /** Shell binary to probe; defaults to `$SHELL` then a platform default. */
  shell?: string;
  /** Probe timeout in ms (default {@link DEFAULT_TIMEOUT_MS}); on timeout ⇒ null. */
  timeoutMs?: number;
}

/**
 * Union two PATH strings: login-shell entries first (so Homebrew/asdf/etc. win
 * precedence, matching the operator's terminal), then any current entries the
 * shell did not already list (so the daemon never loses the dir it was launched
 * from — e.g. the `.app` Resources dir holding the `autoskd` sidecar).
 * Blank/duplicate entries are dropped.
 */
export function mergePathEntries(current: string, shellPath: string, sep: string = PATH_DELIM): string {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const raw of [...shellPath.split(sep), ...current.split(sep)]) {
    const part = raw.trim();
    if (part.length === 0 || seen.has(part)) continue;
    seen.add(part);
    out.push(part);
  }
  return out.join(sep);
}

/**
 * Decide the enriched PATH and how many genuinely new dirs the login shell
 * contributed. When `added === 0` the shell offered nothing the daemon lacks, so
 * the caller should leave `process.env.PATH` untouched rather than reorder it —
 * a terminal-launched daemon (already-rich PATH) is thus a true no-op.
 */
export function computeEnrichedPath(
  current: string,
  shellPath: string,
  sep: string = PATH_DELIM,
): { path: string; added: number } {
  const path = mergePathEntries(current, shellPath, sep);
  const currentSet = new Set(
    current
      .split(sep)
      .map((p) => p.trim())
      .filter((p) => p.length > 0),
  );
  const added = path.split(sep).filter((p) => p.length > 0 && !currentSet.has(p)).length;
  return { path, added };
}

/**
 * Extract the PATH printed between the sentinel markers, tolerating arbitrary
 * shell-startup noise (motd, `echo`s in rc files) before/around it. Returns
 * null when the markers are absent or wrap an empty string.
 */
export function parseShellPathOutput(raw: string): string | null {
  const first = raw.indexOf(MARKER);
  if (first < 0) return null;
  const start = first + MARKER.length;
  const end = raw.indexOf(MARKER, start);
  if (end < 0) return null;
  const path = raw.slice(start, end);
  return path.length > 0 ? path : null;
}

/** Login shell to probe: `$SHELL`, else a platform default. */
function defaultShell(): string {
  const env = process.env.SHELL;
  if (env && env.length > 0) return env;
  return process.platform === "darwin" ? "/bin/zsh" : "/bin/bash";
}

/**
 * Run the operator's login shell and capture its `$PATH`. Resolves null on any
 * failure (spawn error, missing markers, non-zero exit, timeout) — the caller
 * then keeps the inherited PATH. Never rejects.
 *
 * The shell runs interactive+login (`-ilc`) so it sources the same files the
 * operator's terminal does (`.zprofile` AND `.zshrc`, where nvm/asdf usually
 * live). PATH is printed via `printf` wrapped in {@link MARKER}s so we can fish
 * it out of any surrounding rc-file chatter.
 */
export function queryLoginShellPath(opts: QueryOptions = {}): Promise<string | null> {
  return new Promise((resolve) => {
    const shell = opts.shell ?? defaultShell();
    const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
    // Literal markers around an expanded, double-quoted $PATH (single arg).
    const script = `printf '%s' '${MARKER}'"$PATH"'${MARKER}'`;

    let child: ReturnType<typeof spawn>;
    try {
      child = spawn(shell, ["-ilc", script], {
        stdio: ["ignore", "pipe", "ignore"],
        // Quiet oh-my-zsh auto-update prompts and any TTY-gated banners.
        env: { ...process.env, DISABLE_AUTO_UPDATE: "true", TERM: "dumb" },
      });
    } catch {
      resolve(null);
      return;
    }

    let out = "";
    let settled = false;
    const finish = (value: string | null): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        child.kill("SIGKILL");
      } catch {
        // already gone
      }
      resolve(value);
    };
    const timer = setTimeout(() => finish(null), timeoutMs);
    child.stdout?.on("data", (chunk) => {
      out += chunk.toString();
    });
    child.on("error", () => finish(null));
    child.on("close", () => finish(parseShellPathOutput(out)));
  });
}

/** True when this run should NOT probe the login shell. */
function shouldSkip(): boolean {
  if (process.platform === "win32") return true;
  // Bun sets NODE_ENV=test under `bun test`: keep the unit suites hermetic/fast.
  if (process.env.NODE_ENV === "test") return true;
  const skip = process.env.AUTOSK_SKIP_SHELL_PATH;
  return !!skip && skip !== "0";
}

/**
 * Merge the login shell's PATH into `process.env.PATH` so every child the daemon
 * spawns inherits the operator's full toolchain. No-op on Windows, under
 * `bun test`, when `AUTOSK_SKIP_SHELL_PATH` is set, or when the probe yields
 * nothing new — so it is safe (and idempotent) to call once at startup.
 */
export async function enrichProcessPath(logger: Logger, opts: QueryOptions = {}): Promise<void> {
  if (shouldSkip()) return;
  const shellPath = await queryLoginShellPath(opts);
  if (!shellPath) return;
  const { path, added } = computeEnrichedPath(process.env.PATH ?? "", shellPath);
  if (added === 0) return;
  process.env.PATH = path;
  logger.info(`autoskd: PATH enriched from login shell (+${added} entries)`);
}
