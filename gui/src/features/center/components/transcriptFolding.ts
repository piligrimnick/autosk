// transcriptFolding — pure helpers backing the collapsible transcript blocks
// (plan docs/plans/20260628-gui-transcript-folding.md §4.2/§4.1). Kept free of
// React/JSX so they can be unit-tested in the node-environment vitest suite
// (the correlation pre-pass in Transcript.tsx calls these). No DOM, no deps.

import type { ToolResultMessage, TranscriptLine, TranscriptMessage } from "@/types";

/**
 * The collapsed one-line preview of a thinking block: the first non-empty,
 * trimmed line of `text`, truncated with an ellipsis when it overflows `max`.
 * Returns "" for empty / whitespace-only input (the caller falls back to the
 * bare `thinking` label).
 */
export function previewLine(text: string, max = 120): string {
  let line = "";
  for (const raw of (text ?? "").split("\n")) {
    const trimmed = raw.trim();
    if (trimmed.length > 0) {
      line = trimmed;
      break;
    }
  }
  if (line.length <= max) return line;
  return line.slice(0, Math.max(0, max - 1)).trimEnd() + "…";
}

/**
 * Index every standalone `toolResult` transcript line by its `toolCallId` so a
 * merged `ToolBlock` can fold its result in. If a call somehow has multiple
 * results, the last one wins (acceptable per plan §6).
 */
export function indexResults(lines: TranscriptLine[]): Map<string, ToolResultMessage> {
  const map = new Map<string, ToolResultMessage>();
  for (const line of lines) {
    if (line.type === "message" && line.message.role === "toolResult") {
      map.set(line.message.toolCallId, line.message);
    }
  }
  return map;
}

/**
 * Collect every `toolCall.id` that appears in an assistant message (committed
 * lines plus the ephemeral `partial`) — i.e. every call we render as a merged
 * `ToolBlock`. The renderer skips a standalone `toolResult` row whose
 * `toolCallId` is in this set (its content now lives in the call's block); an
 * orphan result keeps its fallback row.
 */
export function collectConsumedCallIds(
  lines: TranscriptLine[],
  partial?: TranscriptMessage | null,
): Set<string> {
  const ids = new Set<string>();
  const scan = (message: TranscriptMessage | null | undefined) => {
    if (!message || message.role !== "assistant") return;
    for (const block of message.content) {
      if (block.type === "toolCall") ids.add(block.id);
    }
  };
  for (const line of lines) {
    if (line.type === "message") scan(line.message);
  }
  scan(partial);
  return ids;
}
