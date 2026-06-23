#!/usr/bin/env bun
/**
 * `stub-claude` — a test stand-in for `claude -p --output-format stream-json
 * --input-format stream-json` (the analogue of pi-agent's `stub-pi`). It speaks
 * the Claude Code stream-json subset the {@link ClaudeDriver} relies on; the
 * claude CLI flags (`-p --model … --mcp-config …`) are ignored.
 *
 * Its scenario is read from `<cwd>/.stub-claude.json` (the daemon spawns claude
 * with `cwd = ctx.cwd`, i.e. the project root in these tests) — a config FILE,
 * not env, because `Bun.spawn` does not propagate a parent's runtime-mutated
 * `process.env` to the child.
 *
 *   { "scenario": "transit" | "kickback_then_transit" | "never_transit"
 *                 | "steer" | "abort_hang",
 *     "to": "<transit target>",            // default "done"
 *     "transitOnTurn": <1-based turn> }    // default 2 (kickback scenario)
 */

import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

interface StubConfig {
  scenario: string;
  to: string;
  transitOnTurn: number;
}

function loadConfig(): StubConfig {
  const path = join(process.cwd(), ".stub-claude.json");
  let raw: Partial<StubConfig> = {};
  if (existsSync(path)) {
    try {
      raw = JSON.parse(readFileSync(path, "utf8")) as Partial<StubConfig>;
    } catch {
      /* defaults */
    }
  }
  return { scenario: raw.scenario ?? "transit", to: raw.to ?? "done", transitOnTurn: raw.transitOnTurn ?? 2 };
}

const cfg = loadConfig();

function emit(obj: unknown): void {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

// The CLI emits a system/init event at startup; the driver records its session_id.
emit({ type: "system", subtype: "init", session_id: "stub-claude-sess", model: "stub-model", mcp_servers: [{ name: "autosk", status: "connected" }] });

let turn = 0;
let toolSeq = 0;

function assistantText(text: string): void {
  emit({
    type: "assistant",
    message: {
      id: `msg_${turn}`,
      role: "assistant",
      model: "stub-model",
      content: [{ type: "text", text }],
      stop_reason: "end_turn",
      usage: { input_tokens: 1, output_tokens: 1 },
    },
  });
}

function emitTransit(to: string): void {
  toolSeq += 1;
  const id = `tu_${toolSeq}`;
  emit({
    type: "assistant",
    message: {
      id: `msg_${turn}_t`,
      role: "assistant",
      model: "stub-model",
      content: [{ type: "tool_use", id, name: "mcp__autosk__transit", input: { to } }],
      stop_reason: "tool_use",
      usage: { input_tokens: 1, output_tokens: 1 },
    },
  });
  emit({
    type: "user",
    message: { role: "user", content: [{ type: "tool_result", tool_use_id: id, content: `autosk: transition to "${to}" submitted.`, is_error: false }] },
  });
}

function result(): void {
  emit({ type: "result", subtype: "success", is_error: false, total_cost_usd: 0, num_turns: turn, usage: { input_tokens: 1, output_tokens: 1 } });
}

/** Runs one turn in response to a user message. */
function runTurn(message: string): void {
  turn += 1;
  // Echo the user message back (mirrors --replay-user-messages).
  emit({ type: "user", message: { role: "user", content: [{ type: "text", text: message }] } });
  assistantText(`ack: ${message}`);

  switch (cfg.scenario) {
    case "transit":
      emitTransit(cfg.to);
      result();
      return;
    case "kickback_then_transit":
      if (turn >= cfg.transitOnTurn) emitTransit(cfg.to);
      result();
      return;
    case "never_transit":
      result();
      return;
    case "steer":
      // Turn 1 hangs (no result → the driver stays streaming), so a forwarded
      // steer arrives as a real interrupt + user message; later turns transit.
      if (turn === 1) return;
      emitTransit(cfg.to);
      result();
      return;
    case "abort_hang":
      return; // hang forever — ended only by abort
    default:
      result();
      return;
  }
}

function userText(message: { content?: unknown }): string {
  const content = message.content;
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .map((b) => (b && typeof b === "object" && (b as { type?: string }).type === "text" ? String((b as { text?: string }).text ?? "") : ""))
      .join("");
  }
  return "";
}

function handle(msg: Record<string, unknown>): void {
  const type = typeof msg.type === "string" ? msg.type : "";
  if (type === "user") {
    runTurn(userText(msg.message as { content?: unknown }));
  }
  // control_request (interrupt) is acknowledged by simply moving on — the next
  // user message starts the new turn.
}

const decoder = new TextDecoder();
let buf = "";
for await (const chunk of Bun.stdin.stream()) {
  buf += decoder.decode(chunk, { stream: true });
  let nl: number;
  while ((nl = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, nl);
    buf = buf.slice(nl + 1);
    const trimmed = line.trim();
    if (trimmed === "") continue;
    try {
      handle(JSON.parse(trimmed) as Record<string, unknown>);
    } catch {
      /* ignore malformed input */
    }
  }
}
