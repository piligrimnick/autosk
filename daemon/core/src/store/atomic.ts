/**
 * Atomic file writes + change signatures (plan §3.7(2)).
 *
 * "Atomic writes everywhere": every mutation writes a sibling temp file in the
 * SAME directory (so the final `rename` is a same-filesystem, atomic swap) and
 * only then renames it over the target. A concurrent reader therefore always
 * sees either the old complete file or the new complete file — never a torn
 * write. We `fsync` the temp file before the rename (its bytes are durable)
 * AND `fsync` the parent directory after the rename (the new directory entry
 * is durable), so a crash cannot resurrect the old name with the new bytes.
 *
 * `fileSig` is the cache key the store stores per file: a
 * `mtimeMs:ctimeMs:size:ino` tuple. Two writes producing the same signature
 * are treated as "the file did not change", which is exactly what powers
 * (a) the mtime-keyed read cache and (b) the watcher's echo suppression — the
 * daemon's own writes update the cached signature, so the watcher event that
 * follows is recognised as self. `ctimeMs` + `ino` widen the signature beyond
 * the bare `mtimeMs:size` pair so an external edit that lands in the same
 * millisecond with the same byte length is still detected: an in-place edit
 * bumps `ctimeMs`, an atomic-rename edit swaps `ino`. The one residual blind
 * spot — an in-place edit in the SAME millisecond with the SAME size AND the
 * SAME ctime as our own write — is astronomically unlikely and accepted.
 */

import { open, mkdir, rename, stat, chmod } from "node:fs/promises";
import type { Stats } from "node:fs";
import { dirname, basename, join } from "node:path";

/** Process-wide counter so concurrent writers never collide on a temp name. */
let tmpCounter = 0;

/**
 * A file change signature: `"<mtimeMs>:<ctimeMs>:<size>:<ino>"`. The store
 * caches this per file and compares it on the next stat to decide "changed vs
 * unchanged" without re-reading the bytes. `ctimeMs` + `ino` are tiebreakers
 * for the (rare) case of two writes landing in the same millisecond with the
 * same size (see the module header for the residual blind spot).
 */
export function fileSig(st: Pick<Stats, "mtimeMs" | "ctimeMs" | "size" | "ino">): string {
  return `${st.mtimeMs}:${st.ctimeMs}:${st.size}:${st.ino}`;
}

/** `stat` + {@link fileSig}, or `null` when the file does not exist. */
export async function statSig(path: string): Promise<{ sig: string; stat: Stats } | null> {
  try {
    const st = await stat(path);
    return { sig: fileSig(st), stat: st };
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === "ENOENT") return null;
    throw e;
  }
}

export interface AtomicWriteOptions {
  /** Mode for the final file (e.g. `0o600`). Applied exactly (umask-proof). */
  mode?: number;
  /** Mode for any directories created on the way (e.g. `0o700`). */
  dirMode?: number;
  /** Skip `fsync` (tests that value speed over durability may set this). */
  noFsync?: boolean;
}

/**
 * Writes `data` to `path` atomically (temp-in-same-dir + fsync + rename) and
 * returns the resulting file's change signature, so the caller can seed its
 * cache and suppress the watcher echo of this very write.
 */
export async function atomicWrite(
  path: string,
  data: string | Uint8Array,
  opts: AtomicWriteOptions = {},
): Promise<string> {
  const dir = dirname(path);
  await mkdir(dir, { recursive: true });
  if (opts.dirMode !== undefined) {
    await chmod(dir, opts.dirMode).catch(() => {});
  }

  const tmp = join(dir, `.${basename(path)}.tmp.${process.pid}.${tmpCounter++}`);
  const fh = await open(tmp, "w", opts.mode ?? 0o644);
  try {
    await fh.writeFile(data);
    // `open(..., mode)` is masked by umask; force the exact mode.
    if (opts.mode !== undefined) await fh.chmod(opts.mode);
    if (!opts.noFsync) await fh.sync();
  } finally {
    await fh.close();
  }
  await rename(tmp, path);

  // Make the rename itself durable: fsync the parent directory so the new
  // directory entry survives a crash, not just the file's bytes. Best-effort —
  // some platforms (notably Windows) reject opening/fsyncing a directory; a
  // failure there leaves the rename as durable as the OS makes it on its own.
  if (!opts.noFsync) {
    try {
      const dirFh = await open(dir, "r");
      try {
        await dirFh.sync();
      } finally {
        await dirFh.close();
      }
    } catch {
      /* directory fsync unsupported on this platform — ignore */
    }
  }

  const st = await stat(path);
  return fileSig(st);
}

/**
 * Appends `data` to `path` and returns the file's new signature.
 *
 * Appends are NOT atomic against a concurrent reader the way a rename is, but
 * the store only ever appends single, newline-terminated JSON lines (transcript
 * entries) while holding the per-file lock, so a reader splitting on `\n` never
 * observes a half-written line. Edits/deletes that must rewrite the whole file
 * go through {@link atomicWrite} instead.
 */
export async function appendLine(path: string, data: string): Promise<string> {
  await mkdir(dirname(path), { recursive: true });
  const fh = await open(path, "a");
  try {
    await fh.writeFile(data);
    await fh.sync();
  } finally {
    await fh.close();
  }
  const st = await stat(path);
  return fileSig(st);
}
