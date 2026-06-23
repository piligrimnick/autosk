/**
 * TCP auth token (plan §4 / §3.7(5)): `~/.autosk/daemon-token`, mode `0600`.
 *
 * The token gates the TCP transport only — UDS connections are exempt (the
 * socket is `0600` in a `0700` dir, so filesystem perms already authenticate the
 * local peer). On startup the daemon reads the token, minting a fresh random
 * one if the file is absent. Path: `$AUTOSK_TOKEN_FILE` → `~/.autosk/daemon-token`.
 */

import { randomBytes } from "node:crypto";
import { chmodSync, closeSync, mkdirSync, openSync, readFileSync, writeSync } from "node:fs";
import { dirname, join } from "node:path";

/** Resolves the token path: `$AUTOSK_TOKEN_FILE` → `~/.autosk/daemon-token`. */
export function resolveTokenPath(): string | null {
  const env = process.env.AUTOSK_TOKEN_FILE;
  if (env && env.length > 0) return env;
  const home = process.env.HOME;
  if (!home || home.length === 0) return null;
  return join(home, ".autosk", "daemon-token");
}

/**
 * Reads the token at `path`, minting + persisting a fresh one (`0600`) if the
 * file is absent or empty. The file is created `0600` from the start so the
 * secret never briefly exists with looser umask perms.
 */
export function ensureToken(path: string): string {
  try {
    const existing = readFileSync(path, "utf8").trim();
    if (existing.length > 0) return existing;
  } catch {
    // absent — mint below
  }
  const token = randomBytes(32).toString("hex");
  const dir = dirname(path);
  mkdirSync(dir, { recursive: true });
  try {
    chmodSync(dir, 0o700);
  } catch {
    // best-effort
  }
  const fd = openSync(path, "w", 0o600);
  try {
    writeSync(fd, `${token}\n`);
  } finally {
    closeSync(fd);
  }
  try {
    chmodSync(path, 0o600);
  } catch {
    // best-effort (in case the file pre-existed empty with wider perms)
  }
  return token;
}
