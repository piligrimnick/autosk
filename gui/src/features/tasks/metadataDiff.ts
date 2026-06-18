// Pure helpers for the GUI task-metadata editor (plan §9). Kept free of React /
// Tauri imports so they unit-test in isolation: the edit modal seeds a textarea
// with `metadataToText` and, on save, diffs the edited document against the
// task's current metadata with `computeMetadataDiff` to produce the set-patch +
// unset-list sent over the wire (no whole-document replace).

/** Renders a metadata bag as the pretty-JSON document the textarea is seeded with. */
export function metadataToText(meta: Record<string, unknown> | undefined): string {
  return JSON.stringify(meta ?? {}, null, 2);
}

/**
 * Parses the edited metadata document (which MUST be a JSON object) and diffs it
 * against `old` at top-level-key granularity, returning the set-patch
 * (added/changed keys) + the unset-list (removed keys). Throws on invalid JSON
 * or a non-object document. An empty/whitespace document means "clear all".
 */
export function computeMetadataDiff(
  old: Record<string, unknown>,
  editedText: string,
): { patch: Record<string, unknown>; unset: string[] } {
  const trimmed = editedText.trim();
  const next: Record<string, unknown> =
    trimmed === "" ? {} : (JSON.parse(trimmed) as Record<string, unknown>);
  if (typeof next !== "object" || next === null || Array.isArray(next)) {
    throw new Error("metadata must be a JSON object");
  }
  const patch: Record<string, unknown> = {};
  for (const key of Object.keys(next)) {
    if (!(key in old) || JSON.stringify(old[key]) !== JSON.stringify(next[key])) {
      patch[key] = next[key];
    }
  }
  const unset: string[] = [];
  for (const key of Object.keys(old)) {
    if (!(key in next)) unset.push(key);
  }
  return { patch, unset };
}
