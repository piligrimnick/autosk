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
 *  1. Write our pid to a private temp file, then atomically `link()` it onto
 *     `<sock>.lock`. `link` fails with `EEXIST` when the lock exists, so
 *     exactly one racer wins — and, crucially, the lock file the instant it is
 *     visible at its final path ALREADY carries a valid pid. (An earlier
 *     `open("wx")`+`write` claim left a window where the lock existed but was
 *     still empty; a racer reading it then misread `""` as a dead holder and
 *     wrongly reclaimed a live lock — letting two daemons bind the same
 *     socket. The link makes content atomic with existence and closes that
 *     window.)
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

import { chmodSync, linkSync, mkdirSync, readFileSync, unlinkSync, writeFileSync } from "node:fs";
import net from "node:net";
import { dirname } from "node:path";

/** Thrown by {@link listenUnix} when another daemon already holds the socket. */
export class AlreadyRunningError extends Error {
  constructor(path: string) {
    super(`daemon already running at ${path}`);
    this.name = "AlreadyRunningError";
  }
}

/** A single-instance listener (lock claimed) plus its lifecycle hooks. */
export interface UnixListenHandle {
  server: net.Server;
  /** The socket path the lock guards. */
  path: string;
  /**
   * Begins accepting connections (binds the socket, `chmod 0600`s it). Call
   * this AFTER attaching the server's `"connection"` handler so a client that
   * connects the instant the socket appears is never dropped (a `net.Server`
   * silently discards `"connection"` events emitted before a listener exists,
   * which left concurrent first-callers hanging on a request that the daemon
   * never read). Rejecting unwinds the lock.
   */
  listen(): Promise<void>;
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
  // A private staging file we link into place; unique per process so concurrent
  // racers never collide on it.
  const lockTmp = `${lockPath}.${process.pid}.tmp`;
  let claimed = false;
  for (let attempt = 0; attempt < 8 && !claimed; attempt++) {
    writeFileSync(lockTmp, `${process.pid}\n`, { mode: 0o600 });
    try {
      // Atomic claim: link() fails EEXIST when the lock is held. On success the
      // lock file already carries our pid (no empty-file window).
      linkSync(lockTmp, lockPath);
      claimed = true;
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "EEXIST") {
        tryUnlink(lockTmp);
        throw e;
      }
      // Someone holds the lock. Alive ⇒ already running; dead ⇒ stale, reclaim.
      if (holderAlive(lockPath)) {
        tryUnlink(lockTmp);
        throw new AlreadyRunningError(socketPath);
      }
      tryUnlink(lockPath);
      tryUnlink(socketPath);
      // loop and re-attempt the atomic claim
    } finally {
      // The lock now lives at lockPath (a hardlink to the same inode); the
      // staging name is no longer needed either way.
      tryUnlink(lockTmp);
    }
  }
  if (!claimed) {
    throw new Error(`could not acquire daemon lock at ${lockPath}`);
  }

  // We hold the lock: reap any stale socket file. The actual bind is deferred
  // to listen() so the caller can wire the connection handler first.
  tryUnlink(socketPath);
  const server = net.createServer();

  const listen = async (): Promise<void> => {
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
      tryUnlink(lockPath);
      throw e;
    }
  };

  let released = false;
  return {
    server,
    path: socketPath,
    listen,
    release: () => {
      if (released) return;
      released = true;
      try {
        server.close();
      } catch {
        /* already closing */
      }
      tryUnlink(socketPath);
      tryUnlink(lockPath);
    },
  };
}
