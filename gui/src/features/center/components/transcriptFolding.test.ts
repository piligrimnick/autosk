import { describe, expect, it } from "vitest";

import type {
  AssistantMessage,
  ContentBlock,
  MessageEntry,
  ToolResultMessage,
  TranscriptLine,
} from "@/types";
import { collectConsumedCallIds, indexResults, previewLine } from "./transcriptFolding";

// ---- builders (only the fields the pure helpers read are populated) --------

let seq = 0;
const id = () => `x${(seq++).toString(16)}`;

function assistantLine(content: ContentBlock[]): MessageEntry {
  return {
    type: "message",
    id: id(),
    timestamp: "t",
    message: { role: "assistant", content } as AssistantMessage,
  };
}

function resultLine(toolCallId: string, text = "ok", isError = false): MessageEntry {
  const message: ToolResultMessage = {
    role: "toolResult",
    toolCallId,
    toolName: "read",
    content: [{ type: "text", text }],
    isError,
    timestamp: 0,
  };
  return { type: "message", id: id(), timestamp: "t", message };
}

const call = (callId: string): ContentBlock => ({
  type: "toolCall",
  id: callId,
  name: "read",
  arguments: {},
});

describe("previewLine", () => {
  it("returns the first non-empty trimmed line", () => {
    expect(previewLine("\n\n  hello world  \nsecond")).toBe("hello world");
  });

  it("returns '' for empty / whitespace-only input", () => {
    expect(previewLine("")).toBe("");
    expect(previewLine("   \n\t\n ")).toBe("");
  });

  it("truncates with an ellipsis when it overflows max", () => {
    const out = previewLine("abcdefghij", 5);
    expect(out).toBe("abcd…");
    expect(out.length).toBe(5);
  });

  it("keeps lines at or under max intact", () => {
    expect(previewLine("abcde", 5)).toBe("abcde");
  });
});

describe("indexResults", () => {
  it("indexes toolResult lines by toolCallId", () => {
    const lines: TranscriptLine[] = [
      assistantLine([call("c1")]),
      resultLine("c1", "first"),
      resultLine("c2", "second"),
    ];
    const map = indexResults(lines);
    expect(map.size).toBe(2);
    expect(map.get("c1")?.content[0]).toMatchObject({ text: "first" });
    expect(map.get("c2")?.content[0]).toMatchObject({ text: "second" });
  });

  it("keeps the last result when a call id repeats", () => {
    const lines: TranscriptLine[] = [resultLine("c1", "old"), resultLine("c1", "new")];
    expect(indexResults(lines).get("c1")?.content[0]).toMatchObject({ text: "new" });
  });

  it("ignores non-result lines", () => {
    expect(indexResults([assistantLine([call("c1")])]).size).toBe(0);
  });
});

describe("collectConsumedCallIds", () => {
  it("collects every toolCall id from assistant lines", () => {
    const lines: TranscriptLine[] = [
      assistantLine([{ type: "text", text: "hi" }, call("c1"), call("c2")]),
      resultLine("c1"),
    ];
    const consumed = collectConsumedCallIds(lines);
    expect([...consumed].sort()).toEqual(["c1", "c2"]);
  });

  it("includes calls from the ephemeral partial assistant message", () => {
    const partial = { role: "assistant", content: [call("p1")] } as AssistantMessage;
    const consumed = collectConsumedCallIds([assistantLine([call("c1")])], partial);
    expect([...consumed].sort()).toEqual(["c1", "p1"]);
  });

  it("ignores results and partials that are not assistant messages", () => {
    const consumed = collectConsumedCallIds([resultLine("c1")], null);
    expect(consumed.size).toBe(0);
  });
});
