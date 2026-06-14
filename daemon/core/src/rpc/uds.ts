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
 *     dead pid ⇒ a stale lock from a crashed daemon, which is reclaimed —
 *     but only UNDER an exclusive `<sock>.lock.break` breaker file so two
 *     concurrent reclaimers can't both delete the lock and double-bind. The
 *     blind `unlink` had a TOCTOU: a sibling could reclaim + link a LIVE lock
 *     between the dead-holder check and the unlink, which the racer then
 *     wrongly deleted and bound over. Serialising the reclaim (and re-checking
 *     liveness under the breaker) closes that window.
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

/** The exclusive breaker that serialises stale-lock reclamation. */
function breakPathFor(lockPath: string): string {
  return lockPath + ".break";
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

/**
 * Acquires the exclusive reclaim breaker via an `O_CREAT|O_EXCL` create.
 * Returns `true` when we hold it (the caller MUST release it with tryUnlink),
 * or `false` when a LIVE sibling already holds it (the caller backs off and
 * re-attempts the claim). A breaker left behind by a crashed reclaimer is itself
 * reclaimed via the same pid-liveness probe.
 */
function acquireBreaker(breakPath: string): boolean {
  for (let i = 0; i < 8; i++) {
    try {
      // wx ⇒ O_CREAT|O_EXCL: fails EEXIST while another reclaimer holds it.
      writeFileSync(breakPath, `${process.pid}\n`, { flag: "wx", mode: 0o600 });
      return true;
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "EEXIST") throw e;
      if (holderAlive(breakPath)) return false; // a live reclaimer owns it
      tryUnlink(breakPath); // a crashed reclaimer's breaker — drop it and retry
    }
  }
  return false;
}

/**
 * Reclaims a stale lock at `lockPath` UNDER the exclusive breaker, so two
 * concurrent racers can never both delete the lock and bind the same socket.
 *
 * Without this serialisation there is a TOCTOU between the dead-holder check and
 * the reclaim unlink: a sibling could reclaim + link a LIVE lock in the gap,
 * which the racer would then wrongly delete and bind over — stranding clients on
 * an orphaned listener and leaving two engines owning one `.autosk/`.
 *
 * Returns `"reclaimed"` when the stale lock was removed (the caller re-attempts
 * the atomic link), or `"busy"` when another reclaimer holds the breaker (the
 * caller backs off and retries). Throws {@link AlreadyRunningError} when — while
 * we hold the breaker — the holder turns out to be alive, i.e. a sibling
 * reclaimed first and is now the legitimate single instance.
 */
async function reclaimStaleLock(lockPath: string, socketPath: string): Promise<"reclaimed" | "busy"> {
  const breakPath = breakPathFor(lockPath);
  if (!acquireBreaker(breakPath)) return "busy";
  try {
    // Re-check UNDER the breaker. While we hold it nobody else can remove or
    // replace the lock — a fresh link() fails EEXIST against the still-present
    // stale file, and the only other remover (reclaim) needs this same breaker —
    // so an "alive" holder here is a sibling that legitimately reclaimed first.
    if (holderAlive(lockPath)) throw new AlreadyRunningError(socketPath);
    // Test seam: park INSIDE the critical section — holding the breaker, AFTER
    // the dead-holder re-check, BEFORE the unlink — so a staggered sibling
    // reclaimer must serialise behind us. Without the breaker it would race in
    // here, link a LIVE lock + bind, and we'd then unlink its live lock and
    // double-bind. Only ever set by the single-instance regression test.
    const holdMs = Number(process.env.AUTOSK_UDS_BREAKER_HOLD_MS);
    if (holdMs > 0) await sleep(holdMs);
    tryUnlink(lockPath);
    tryUnlink(socketPath);
    return "reclaimed";
  } finally {
    tryUnlink(breakPath);
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
  // Deadline-bounded so heavy stale-lock contention (every racer serialising on
  // the breaker) still resolves instead of giving up after a fixed attempt cap.
  const deadline = Date.now() + 5_000;
  let claimed = false;
  while (!claimed) {
    writeFileSync(lockTmp, `${process.pid}\n`, { mode: 0o600 });
    try {
      // Atomic claim: link() fails EEXIST when the lock is held. On success the
      // lock file already carries our pid (no empty-file window).
      linkSync(lockTmp, lockPath);
      claimed = true;
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "EEXIST") {
        throw e;
      }
      // Someone holds the lock. Alive ⇒ already running.
      if (holderAlive(lockPath)) {
        throw new AlreadyRunningError(socketPath);
      }
      // Dead holder ⇒ stale: reclaim it under the exclusive breaker so two
      // concurrent racers can't both delete the lock and double-bind.
      if ((await reclaimStaleLock(lockPath, socketPath)) === "busy") {
        await sleep(10); // a sibling is breaking the lock — back off, then retry
      }
      // loop and re-attempt the atomic claim
    } finally {
      // The lock now lives at lockPath (a hardlink to the same inode); the
      // staging name is no longer needed either way.
      tryUnlink(lockTmp);
    }
    if (!claimed && Date.now() > deadline) {
      throw new Error(`could not acquire daemon lock at ${lockPath}`);
    }
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
