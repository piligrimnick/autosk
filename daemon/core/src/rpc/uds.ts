/**
 * Single-instance Unix-domain-socket binding (plan §4 / §3.7(5)).
 *
 * The v1 Rust daemon got single-instance for free: `UnixListener::bind` fails
 * with `EADDRINUSE` when the socket file exists, so the loser of an auto-spawn
 * race detected "already running" and fell back to connecting. Bun's
 * `node:net` (and `Bun.listen`) instead SILENTLY rebind an existing unix path
 * — bind never errors — so that signal is gone. We replace it with an explicit
 * **pidfile lock**:
 *
 *  1. Create `<sock>.lock` atomically with `O_CREAT|O_EXCL` (`open(..,"wx")`)
 *     and write our pid. Exactly one racer wins this create; the rest get
 *     `EEXIST`.
 *  2. An `EEXIST` racer reads the holder's pid and probes liveness with
 *     `process.kill(pid, 0)`: alive ⇒ {@link AlreadyRunningError} (exit 0); a
 *     dead pid ⇒ a stale lock from a crashed daemon, which is reclaimed.
 *  3. The winner reaps any stale socket file, binds, and `chmod 0600`s the
 *     socket (parent dir `0700`), holding the lock fd for its lifetime.
 *
 * The pidfile makes the claim atomic and immune to bind latency (a slow winner
 * is still detected as alive), and it works for an in-process race too (both
 * "processes" share a live pid, so the loser still sees a live holder).
 */

import { chmodSync, closeSync, mkdirSync, openSync, readFileSync, unlinkSync, writeSync } from "node:fs";
import net from "node:net";
import { dirname } from "node:path";

/** Thrown by {@link listenUnix} when another daemon already holds the socket. */
export class AlreadyRunningError extends Error {
  constructor(path: string) {
    super(`daemon already running at ${path}`);
    this.name = "AlreadyRunningError";
  }
}

/** A bound single-instance listener plus its teardown. */
export interface UnixListenHandle {
  server: net.Server;
  /** The bound socket path. */
  path: string;
  /** Closes the server and removes the socket + lock files (idempotent). */
  release(): void;
}

/** The lockfile path that guards `socketPath`. */
function lockPathFor(socketPath: string): string {
  return socketPath + ".lock";
}

/** Whether the pid recorded in `lockPath` belongs to a live process. */
function holderAlive(lockPath: string): boolean {
  let pid: number;
  try {
    pid = Number.parseInt(readFileSync(lockPath, "utf8").trim(), 10);
  } catch {
    return false; // the lock vanished between EEXIST and read — not held
  }
  if (!Number.isInteger(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    // ESRCH ⇒ no such process (stale). EPERM ⇒ the process exists but is owned
    // by another user — still alive, so treat as running.
    return (e as NodeJS.ErrnoException).code === "EPERM";
  }
}

function tryUnlink(path: string): void {
  try {
    unlinkSync(path);
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code !== "ENOENT") throw e;
  }
}

/**
 * Binds a single-instance unix listener at `socketPath`. Throws
 * {@link AlreadyRunningError} when another live daemon holds the lock; any other
 * failure (mkdir / bind / chmod) propagates.
 */
export async function listenUnix(socketPath: string): Promise<UnixListenHandle> {
  const dir = dirname(socketPath);
  if (dir && dir.length > 0) {
    mkdirSync(dir, { recursive: true });
    try {
      chmodSync(dir, 0o700);
    } catch {
      // best-effort: a pre-existing dir we don't own keeps its perms
    }
  }

  const lockPath = lockPathFor(socketPath);
  let lockFd: number | null = null;
  for (let attempt = 0; attempt < 8 && lockFd === null; attempt++) {
    try {
      lockFd = openSync(lockPath, "wx", 0o600);
      writeSync(lockFd, `${process.pid}\n`);
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "EEXIST") throw e;
      // Someone holds the lock. Alive ⇒ already running; dead ⇒ stale, reclaim.
      if (holderAlive(lockPath)) throw new AlreadyRunningError(socketPath);
      tryUnlink(lockPath);
      tryUnlink(socketPath);
      // loop and re-attempt the atomic claim
    }
  }
  if (lockFd === null) {
    throw new Error(`could not acquire daemon lock at ${lockPath}`);
  }

  // We hold the lock: reap any stale socket file, then bind.
  tryUnlink(socketPath);
  const server = net.createServer();
  try {
    await new Promise<void>((resolve, reject) => {
      const onError = (e: unknown) => reject(e);
      server.once("error", onError);
      server.listen(socketPath, () => {
        server.removeListener("error", onError);
        resolve();
      });
    });
    try {
      chmodSync(socketPath, 0o600);
    } catch {
      // best-effort hardening; the parent dir is already 0700
    }
  } catch (e) {
    closeSync(lockFd);
    tryUnlink(lockPath);
    throw e;
  }

  let released = false;
  return {
    server,
    path: socketPath,
    release: () => {
      if (released) return;
      released = true;
      try {
        server.close();
      } catch {
        /* already closing */
      }
      tryUnlink(socketPath);
      try {
        closeSync(lockFd);
      } catch {
        /* already closed */
      }
      tryUnlink(lockPath);
    },
  };
}
