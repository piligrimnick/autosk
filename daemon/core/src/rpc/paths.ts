/**
 * Daemon transport path + tuning knobs (plan §4 / §3.7(5)).
 *
 * These env knobs are introduced by P5 (not referenced anywhere else in the
 * tree): `AUTOSK_SOCK` (UDS path), `AUTOSK_TOKEN_FILE` (see `token.ts`), and
 * `AUTOSK_IDLE_SECS` (idle-shutdown window). The defaults mirror the v1 Rust
 * daemon: `~/.autosk/daemon.sock` and a 30-minute idle window.
 */

import { join } from "node:path";

/** The default idle-shutdown window, in seconds (plan §4: 1800s = 30 min). */
export const DEFAULT_IDLE_SECS = 1800;

/** Resolves the UDS path: explicit override → `$AUTOSK_SOCK` → `~/.autosk/daemon.sock`. */
export function resolveSocketPath(override?: string): string {
  if (override && override.length > 0) return override;
  const env = process.env.AUTOSK_SOCK;
  if (env && env.length > 0) return env;
  const home = process.env.HOME;
  if (!home || home.length === 0) throw new Error("HOME is not set; cannot resolve the daemon socket");
  return join(home, ".autosk", "daemon.sock");
}

/**
 * The idle-shutdown window in milliseconds, from `$AUTOSK_IDLE_SECS`
 * (default {@link DEFAULT_IDLE_SECS}). `0` (and negatives) disable idle-shutdown
 * → `null`.
 */
export function resolveIdleWindowMs(): number | null {
  const env = process.env.AUTOSK_IDLE_SECS;
  if (env === undefined || env === "") return DEFAULT_IDLE_SECS * 1000;
  const secs = Number.parseInt(env, 10);
  if (!Number.isFinite(secs) || secs <= 0) return null;
  return secs * 1000;
}
