#!/usr/bin/env bun
/**
 * `stub-pi` — a test stand-in for `pi --mode rpc` (the v2 analogue of v1's
 * `crates/autosk-core/src/bin/fakepi.rs`). It speaks the JSON-Lines RPC subset
 * the {@link PiDriver} relies on. The pi CLI flags (`--mode rpc -e … --model …`)
 * are ignored.
 *
 * Its scenario is read from `<cwd>/.stub-pi.json` (the autosk daemon spawns pi
 * with `cwd = ctx.cwd`, i.e. the project root). A config FILE — not env vars —
 * is used because `Bun.spawn` does not propagate a parent's runtime-mutated
 * `process.env` to the child, and the file keeps parallel test runs isolated:
 *
 * The stub tracks pi's streaming state (set on `agent_start`, cleared on
 * `agent_end`) and enforces pi's command-state contract: a `prompt` is rejected
 * mid-stream and `steer` / `follow_up` are rejected when idle (both as a
 * state-mismatch `success:false`). This is what makes the driver's idle-vs-
 * streaming `input()` dispatch (and its single state-mismatch retry) testable.
 *
 *   { "scenario": "transit" | "kickback_then_transit" | "never_transit"
 *                 | "steer" | "abort_hang",
 *     "to": "<autosk_transit target>",          // default "done"
 *     "transitOnTurn": <1-based turn>            // default 2 (kickback scenario)
 *   }
 */

import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

interface StubConfig {
  scenario: string;
  to: string;
  transitOnTurn: number;
}

function loadConfig(): StubConfig {
  const path = join(process.cwd(), ".stub-pi.json");
  let raw: Partial<StubConfig> = {};
  if (existsSync(path)) {
    try {
      raw = JSON.parse(readFileSync(path, "utf8")) as Partial<StubConfig>;
    } catch {
      /* fall back to defaults */
    }
  }
  return {
    scenario: raw.scenario ?? "transit",
    to: raw.to ?? "done",
    transitOnTurn: raw.transitOnTurn ?? 2,
  };
}

const cfg = loadConfig();

function emit(obj: unknown): void {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

function assistantMessage(text: string): unknown {
  return {
    role: "assistant",
    content: [{ type: "text", text }],
    provider: "stub",
    model: "stub-model",
    usage: {
      input: 1,
      output: 1,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: 2,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
    stopReason: "stop",
    timestamp: Date.now(),
  };
}

let turn = 0;
let toolCallSeq = 0;
/**
 * Whether a turn is in flight. Set on `agent_start`, cleared on `agent_end`.
 * Models pi's streaming state machine so the stub can reject a `prompt`
 * mid-stream and accept `steer`/`follow_up` ONLY mid-stream (R2b) — the guard
 * that keeps the driver's idle-vs-streaming dispatch (`input()`) honest.
 */
let streaming = false;

function startTurn(): void {
  emit({ type: "agent_start" });
  streaming = true;
}

function endTurn(): void {
  emit({ type: "agent_end", messages: [] });
  streaming = false;
}

function emitTransit(to: string): void {
  toolCallSeq += 1;
  emit({
    type: "tool_execution_start",
    toolCallId: `call-${toolCallSeq}`,
    toolName: "autosk_transit",
    args: { to },
  });
  emit({
    type: "tool_execution_end",
    toolCallId: `call-${toolCallSeq}`,
    toolName: "autosk_transit",
    result: { content: [{ type: "text", text: `autosk: transition to "${to}" submitted.` }] },
    isError: false,
  });
}

/** Runs one "turn" in response to a prompt / steer / follow_up. */
function runTurn(message: string): void {
  turn += 1;
  startTurn();
  emit({ type: "message_end", message: assistantMessage(`ack: ${message}`) });

  switch (cfg.scenario) {
    case "transit":
      emitTransit(cfg.to);
      endTurn();
      return;
    case "kickback_then_transit":
      if (turn >= cfg.transitOnTurn) emitTransit(cfg.to);
      endTurn();
      return;
    case "never_transit":
      endTurn();
      return;
    case "steer":
      // Turn 1 hangs mid-stream (no agent_end → `streaming` stays true). The
      // forwarded steer therefore arrives as a real `steer` command (the
      // driver's streaming branch), NOT a prompt; we run a turn for it, echo its
      // message (proving live delivery into pi), then transit.
      if (turn === 1) return;
      emitTransit(cfg.to);
      endTurn();
      return;
    case "abort_hang":
      // Hang forever — the run is ended only by an abort (signal kill / abort cmd).
      return;
    default:
      endTurn();
      return;
  }
}

function handle(cmd: Record<string, unknown>): void {
  const type = typeof cmd.type === "string" ? cmd.type : "";
  const id = typeof cmd.id === "string" ? cmd.id : "";
  const message = typeof cmd.message === "string" ? cmd.message : "";
  switch (type) {
    case "get_state":
      emit({
        type: "response",
        id,
        command: "get_state",
        success: true,
        data: { sessionId: "stub-sess", sessionFile: "/tmp/stub/session.jsonl", messageCount: 0 },
      });
      return;
    case "prompt":
      // pi accepts a fresh `prompt` ONLY when idle; a prompt issued mid-stream is
      // a state mismatch (models real pi, and guards the driver's idle-vs-
      // streaming dispatch — R2b).
      if (streaming) {
        emit({
          type: "response",
          id,
          command: "prompt",
          success: false,
          error: "agent already streaming (in_progress)",
        });
        return;
      }
      emit({ type: "response", id, command: "prompt", success: true });
      runTurn(message);
      return;
    case "steer":
    case "follow_up":
      // steer / follow_up are valid ONLY mid-stream; when idle they are a state
      // mismatch (the driver then retries with the opposite `prompt` shape).
      if (!streaming) {
        emit({
          type: "response",
          id,
          command: type,
          success: false,
          error: "not streaming (no active run)",
        });
        return;
      }
      emit({ type: "response", id, command: type, success: true });
      runTurn(message);
      return;
    case "abort":
      emit({ type: "response", id, command: "abort", success: true });
      process.exit(0);
      return;
    case "extension_ui_response":
      return;
    default:
      emit({ type: "response", id, command: type, success: true });
      return;
  }
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
      emit({ type: "response", id: "", command: "?", success: false, error: "parse error" });
    }
  }
}
