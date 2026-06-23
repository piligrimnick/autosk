/**
 * Shared id helpers (plan §3.1, §3.2, §6 P1).
 *
 *  - Task ids: `ask-` + 6 lowercase hex chars (unchanged from v1).
 *  - Comment ids: `cm-` + 6 lowercase hex chars (v2 makes comment ids strings;
 *    v1 used autoincrement integers, which die with the SQL store). Because the
 *    comment id is the edit/delete key, `newCommentId` is collision-checked
 *    against a per-task taken set (like `newEntryId`), so two comments on one
 *    task can never share an id.
 *  - Session ids: time-ordered UUIDv7, so on-disk session files sort by
 *    creation time and stay globally unique.
 *  - Transcript entry ids: 8-char hex, collision-checked against a taken set
 *    (mirrors pi's `generateId`).
 *
 * All randomness comes from the Web Crypto global (`crypto`), available in Bun.
 */

function randomHex(bytes: number): string {
  const buf = new Uint8Array(bytes);
  crypto.getRandomValues(buf);
  let out = "";
  for (const b of buf) out += b.toString(16).padStart(2, "0");
  return out;
}

/** A new task id: `ask-<6 hex>`. */
export function newTaskId(): string {
  return `ask-${randomHex(3)}`;
}

/**
 * A new comment id: `cm-<6 hex>`, collision-checked against `taken` (the comment
 * ids already on the task). The id is the mutation key for edit/delete, so a
 * duplicate within one task would silently retarget the wrong comment — passing
 * the existing ids guarantees uniqueness within the task. Falls back to a wider
 * id only after 100 straight collisions (astronomically unlikely).
 */
export function newCommentId(taken?: TakenIds): string {
  for (let i = 0; i < 100; i++) {
    const id = `cm-${randomHex(3)}`;
    if (!isTaken(taken, id)) return id;
  }
  return `cm-${randomHex(8)}`;
}

/**
 * A new session id: a UUIDv7 string (time-ordered, lexicographically sortable
 * by creation time).
 */
export function newSessionId(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);

  // 48-bit big-endian millisecond timestamp in bytes 0..5.
  const ts = Date.now();
  bytes[0] = Math.floor(ts / 2 ** 40) & 0xff;
  bytes[1] = Math.floor(ts / 2 ** 32) & 0xff;
  bytes[2] = Math.floor(ts / 2 ** 24) & 0xff;
  bytes[3] = Math.floor(ts / 2 ** 16) & 0xff;
  bytes[4] = Math.floor(ts / 2 ** 8) & 0xff;
  bytes[5] = ts & 0xff;

  // Version 7 + RFC4122 variant.
  bytes[6] = (bytes[6]! & 0x0f) | 0x70;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;

  let hex = "";
  for (const b of bytes) hex += b.toString(16).padStart(2, "0");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/** A "taken" predicate for {@link newEntryId}: a Set or a membership function. */
export type TakenIds = { has(id: string): boolean } | ((id: string) => boolean);

function isTaken(taken: TakenIds | undefined, id: string): boolean {
  if (!taken) return false;
  return typeof taken === "function" ? taken(id) : taken.has(id);
}

/**
 * A new 8-char hex transcript entry id, collision-checked against `taken`
 * (mirrors pi's `generateId`). Falls back to a longer id on the (astronomically
 * unlikely) event of 100 straight collisions.
 */
export function newEntryId(taken?: TakenIds): string {
  for (let i = 0; i < 100; i++) {
    const id = randomHex(4);
    if (!isTaken(taken, id)) return id;
  }
  return randomHex(16);
}
