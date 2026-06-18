/**
 * Free-form task `metadata` helpers (plan §3).
 *
 * `metadata` is an opaque, human-editable key/value bag persisted in
 * `task.json` (see {@link StoredTask}). The daemon treats it as data EXCEPT for
 * one reserved sub-object, `step_visits` (step name → entry count), which the
 * engine auto-maintains so a workflow's `ctx.visits()` cap reads a persistent,
 * human-resettable counter instead of counting session files.
 *
 * Every helper here is defensive: the bag can be edited by hand under hybrid
 * ownership, so a corrupt/missing/wrong-typed `step_visits` (or a non-numeric
 * entry) is tolerated rather than thrown on.
 *
 * The edit RPCs (`task.metadata.set` / `task.metadata.unset`) address leaves by
 * **dot-path** (e.g. `step_visits.dev`): `set` creates any intermediate objects
 * and writes the leaf; `unset` removes the leaf and prunes parents emptied by
 * the removal.
 */

/** The reserved metadata sub-object the engine auto-maintains (step → count). */
export const STEP_VISITS_KEY = "step_visits";

/** Whether a metadata bag is absent/empty (drives the omit-when-empty serialise). */
export function isEmptyMetadata(meta: Record<string, unknown> | undefined | null): boolean {
  return meta == null || Object.keys(meta).length === 0;
}

/**
 * Reads the reserved `step_visits` map as `{ [step]: count }`, defensively
 * ignoring any non-finite / non-numeric entry. Tolerates the wire's `number`
 * (`float64`) shape — a count round-tripped through JSON stays a plain number
 * (a finite fractional value, e.g. a hand-set `1.5`, is kept as-is; the engine
 * only ever writes integers). Returns a fresh object; never aliases the stored
 * bag.
 */
export function getStepVisits(meta: Record<string, unknown> | undefined | null): Record<string, number> {
  const out: Record<string, number> = {};
  if (meta == null) return out;
  const raw = meta[STEP_VISITS_KEY];
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return out;
  for (const [step, value] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof value === "number" && Number.isFinite(value)) out[step] = value;
  }
  return out;
}

/**
 * Increments `meta.step_visits[step]` by one (creating the reserved sub-object
 * and/or the entry as needed), mutating `meta` in place. A pre-existing
 * non-object `step_visits` or non-numeric entry is reset rather than trusted, so
 * a hand-corrupted counter self-heals to a clean count on the next bump.
 */
export function bumpStepVisit(meta: Record<string, unknown>, step: string): void {
  const current = getStepVisits(meta);
  current[step] = (current[step] ?? 0) + 1;
  meta[STEP_VISITS_KEY] = current;
}

/**
 * Applies a dot-path merge patch to `meta` in place. Each entry's KEY is a
 * dot-path (`a.b.c`); the value is written at that leaf, creating intermediate
 * plain objects along the way. A path segment whose existing value is not a
 * plain object is overwritten with a fresh object so the deeper write can land.
 */
export function applyMetadataPatch(
  meta: Record<string, unknown>,
  patch: Record<string, unknown>,
): void {
  for (const [path, value] of Object.entries(patch)) {
    setDotPath(meta, splitPath(path), value);
  }
}

/**
 * Removes each dot-path key from `meta` in place, then prunes any ancestor
 * object emptied by the removal (so `unset step_visits.dev` of the last entry
 * also drops the now-empty `step_visits`). Unknown paths are a no-op.
 */
export function applyMetadataUnset(meta: Record<string, unknown>, keys: string[]): void {
  for (const key of keys) {
    deleteDotPath(meta, splitPath(key));
  }
}

// ---------------------------------------------------------------------------
// Dot-path internals.
// ---------------------------------------------------------------------------

/** Splits a dot-path into its non-empty segments (a leading/trailing dot is ignored). */
function splitPath(path: string): string[] {
  return path.split(".").filter((s) => s.length > 0);
}

/** Whether `v` is a plain (non-array, non-null) object we can descend into. */
function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** Writes `value` at the segment path, creating intermediate objects. No-op on an empty path. */
function setDotPath(root: Record<string, unknown>, segments: string[], value: unknown): void {
  if (segments.length === 0) return;
  let node = root;
  for (let i = 0; i < segments.length - 1; i++) {
    const seg = segments[i]!;
    const next = node[seg];
    if (!isPlainObject(next)) {
      const created: Record<string, unknown> = {};
      node[seg] = created;
      node = created;
    } else {
      node = next;
    }
  }
  node[segments[segments.length - 1]!] = value;
}

/** Deletes the leaf at the segment path, pruning each parent left empty by the delete. */
function deleteDotPath(root: Record<string, unknown>, segments: string[]): void {
  if (segments.length === 0) return;
  // Record the descent so we can prune empties on the way back up.
  const chain: Record<string, unknown>[] = [root];
  let node = root;
  for (let i = 0; i < segments.length - 1; i++) {
    const next = node[segments[i]!];
    if (!isPlainObject(next)) return; // path does not exist — nothing to delete
    node = next;
    chain.push(node);
  }
  delete node[segments[segments.length - 1]!];
  // Walk back up pruning any object emptied by the removal (but never the root).
  for (let i = chain.length - 1; i >= 1; i--) {
    const obj = chain[i]!;
    if (Object.keys(obj).length > 0) break;
    delete chain[i - 1]![segments[i - 1]!];
  }
}
