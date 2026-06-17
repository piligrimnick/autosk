/**
 * `pi --mode rpc` wire driver — the v2 TypeScript port of v1's `pi --mode rpc`
 * driver.
 *
 * Drives a spawned `pi --mode rpc` child over JSON-Lines stdio via the engine's
 * {@link ChildHandle} (`ctx.spawn`). It:
 *  - sends commands (`prompt`, `steer`, `abort`, …) and matches `response` lines
 *    by `id`;
 *  - tracks the `agent_start`/`agent_end` streaming flag and turn boundaries;
 *  - observes the `autosk_transit` tool call on the event stream and exposes the
 *    requested {@link StepTarget} to the agent (the design's transit channel —
 *    "observe the tool call on pi's RPC stream and translate to `ctx.transit`",
 *    plan §3.4, resolved-#2);
 *  - mirrors pi message / custom session entries into the autosk transcript 1:1;
 *  - auto-cancels blocking `extension_ui_request` dialogs so headless runs never
 *    hang.
 */

import type { ChildHandle, StepTarget, TranscriptMessage } from "@autosk/sdk";

/** How a turn (one `prompt` cycle) ended. */
export type TurnEnd = "ended" | "exited" | "aborted";

/** Hooks the agent wires into the driver. */
export interface PiDriverHooks {
  /** Mirror a pi message-schema entry into the transcript (`ctx.log.message`). */
  onMessage(message: TranscriptMessage): void;
  /** Mirror a pi custom session entry into the transcript (`ctx.log.custom`). */
  onCustom(customType: string, data: unknown): void;
  /** The session's abort signal (`ctx.signal`). */
  signal: AbortSignal;
  /**
   * Turn-boundary activity callback: `true` when pi starts streaming a turn
   * (`agent_start`), `false` when the turn ends (`agent_end`). The interactive
   * chat loop wires this to `ctx.setActivity` so a client shows idle vs working.
   */
  onActivity?(busy: boolean): void;
  /** Optional diagnostic sink. */
  warn?(message: string): void;
}

/** pi RPC extension-UI methods that expect no response (fire-and-forget). */
const FIRE_AND_FORGET = new Set(["notify", "setStatus", "setWidget", "setTitle", "set_editor_text"]);

/** The tool name the injected pi extension registers (see `pi-transit-extension.ts`). */
export const TRANSIT_TOOL_NAME = "autosk_transit";

/** Cap on how many pi stderr lines are forwarded through `warn` per session. */
const STDERR_FORWARD_CAP = 100;

interface PendingResponse {
  resolve(value: { success: boolean; error?: string; data?: unknown }): void;
}

export class PiDriver {
  private nextId = 0;
  private readonly pending = new Map<string, PendingResponse>();
  private streaming = false;
  private exited = false;
  private aborted = false;
  private shuttingDown = false;
  exitCode: number | null = null;

  /** Buffered turn-end events (so an `agent_end` that races the await isn't lost). */
  private readonly turnQueue: TurnEnd[] = [];
  private turnResolve: ((r: TurnEnd) => void) | null = null;

  /** The transit target observed during the current turn, or `null`. */
  private pendingTarget: StepTarget | null = null;

  /** How many pi stderr lines have already been forwarded through `warn`. */
  private stderrForwarded = 0;

  constructor(
    private readonly child: ChildHandle,
    private readonly hooks: PiDriverHooks,
  ) {
    child.onStdout((line) => this.onLine(line));
    // Always subscribe (so the pipe is drained and never fills), but forward the
    // bytes through `warn` instead of black-holing them: when pi dies on a
    // runtime/module error its stack trace lives ONLY on stderr.
    child.onStderr((line) => this.onStderrLine(line));
    void child.exited.then(({ code }) => {
      this.exitCode = code;
      this.exited = true;
      for (const p of this.pending.values()) p.resolve({ success: false, error: "pi exited" });
      this.pending.clear();
      this.emitTurn(this.aborted ? "aborted" : "exited");
    });
    if (hooks.signal.aborted) this.onAbort();
    else hooks.signal.addEventListener("abort", () => this.onAbort(), { once: true });
  }

  // -- outbound ------------------------------------------------------------

  /** Sends a `prompt`; resolves once pi acks acceptance. Throws if rejected. */
  async sendPrompt(message: string): Promise<void> {
    const resp = await this.request({ type: "prompt", message });
    if (!resp.success) throw new Error(`pi rejected prompt: ${resp.error ?? "unknown"}`);
  }

  /**
   * Forwards a steer/followup into the live pi (plan §3.4), mirroring v1's
   * `dispatch_input` / `build_input_command` (crates/autoskd/src/server.rs). The
   * pi input command TYPE depends on whether pi is currently streaming:
   *  - idle (no active run)  → `{ type: "prompt", message }` — starts a fresh turn;
   *  - streaming + steer     → `{ type: "steer", message }`;
   *  - streaming + followup  → `{ type: "follow_up", message }` (snake_case — the
   *    real pi command type, NOT a `streamingBehavior` field on `prompt`, which
   *    pi rejects mid-stream).
   * On a state-mismatch rejection (our streaming view raced pi's), retry ONCE
   * with the opposite dispatch shape. Best-effort — never throws; failures are
   * surfaced through `warn`.
   */
  async input(kind: "steer" | "followup", message: string): Promise<void> {
    try {
      const streaming = this.streaming;
      const first = buildInputCommand(kind, message, streaming);
      const resp = await this.request(first.cmd);
      if (resp.success) return;
      if (!isStateMismatch(resp.error)) {
        this.hooks.warn?.(`pi-agent: pi rejected ${first.label} (${resp.error ?? "unknown"})`);
        return;
      }
      // State-mismatch: our streaming view raced pi's — retry once with the
      // opposite dispatch shape (v1 `dispatch_input`).
      const retry = buildInputCommand(kind, message, !streaming);
      const retryResp = await this.request(retry.cmd);
      if (retryResp.success) return;
      this.hooks.warn?.(
        `pi-agent: pi rejected ${retry.label} after retry from ${first.label} (${retryResp.error ?? "unknown"})`,
      );
    } catch (e) {
      this.hooks.warn?.(`pi-agent: forwarding ${kind} failed (${errMsg(e)})`);
    }
  }

  /** Waits for the current turn to end (or the child to exit / be aborted). */
  waitForTurnEnd(): Promise<TurnEnd> {
    if (this.turnQueue.length > 0) return Promise.resolve(this.turnQueue.shift()!);
    if (this.aborted) return Promise.resolve("aborted");
    if (this.exited) return Promise.resolve("exited");
    return new Promise<TurnEnd>((resolve) => {
      this.turnResolve = resolve;
    });
  }

  /** Takes (and clears) the transit target observed this turn, if any. */
  takePendingTarget(): StepTarget | null {
    const t = this.pendingTarget;
    this.pendingTarget = null;
    return t;
  }

  /**
   * Graceful pi shutdown: `abort` command, close stdin, brief grace, then kill.
   * Idempotent — both `onAbort` and `onRun`'s finally can call it for one session.
   */
  async shutdown(graceMs = 500): Promise<void> {
    if (this.shuttingDown) return;
    this.shuttingDown = true;
    this.writeRaw({ type: "abort" }); // fire-and-forget; pi may already be gone
    try {
      await this.child.stdin.close();
    } catch {
      /* already closed */
    }
    const timeout = new Promise<void>((r) => setTimeout(r, graceMs));
    await Promise.race([this.child.exited.then(() => undefined), timeout]);
    try {
      this.child.kill();
    } catch {
      /* already dead */
    }
  }

  // -- inbound -------------------------------------------------------------

  /** Forwards a (bounded count of) pi stderr lines through the `warn` sink. */
  private onStderrLine(line: string): void {
    // Strip ANSI/terminal control sequences first: pi writes a burst of these to
    // stderr on teardown (disable mouse tracking, leave the alt-screen, end
    // synchronized output, …) even under `--mode rpc`, and they'd otherwise land
    // verbatim as an unreadable final `pi:stderr:` line in the transcript.
    const trimmed = stripAnsi(line).trim();
    if (trimmed === "") return; // pure-control teardown / blank → drained, not surfaced
    if (this.stderrForwarded >= STDERR_FORWARD_CAP) return;
    this.stderrForwarded++;
    this.hooks.warn?.(`pi:stderr: ${trimmed}`);
    if (this.stderrForwarded === STDERR_FORWARD_CAP) {
      this.hooks.warn?.(`pi:stderr: (further stderr suppressed after ${STDERR_FORWARD_CAP} lines)`);
    }
  }

  private onLine(line: string): void {
    const trimmed = line.trim();
    if (trimmed === "") return;
    let msg: Record<string, unknown>;
    try {
      msg = JSON.parse(trimmed) as Record<string, unknown>;
    } catch {
      return; // line-oriented resync: skip a non-JSON line, keep reading
    }
    const type = typeof msg.type === "string" ? msg.type : "";
    switch (type) {
      case "response":
        this.deliverResponse(msg);
        return;
      case "agent_start":
        this.streaming = true;
        this.hooks.onActivity?.(true);
        return;
      case "agent_end":
        this.streaming = false;
        this.hooks.onActivity?.(false);
        this.emitTurn("ended");
        return;
      case "tool_execution_start":
        this.observeToolCall(msg);
        return;
      case "message_start":
      case "message_end":
        if (type === "message_end") this.mirrorMessage(msg.message);
        return;
      case "extension_ui_request":
        this.replyToExtensionUi(msg);
        return;
      default:
        return;
    }
  }

  private deliverResponse(msg: Record<string, unknown>): void {
    const id = typeof msg.id === "string" ? msg.id : "";
    const p = this.pending.get(id);
    if (!p) return;
    this.pending.delete(id);
    p.resolve({
      success: msg.success === true,
      error: typeof msg.error === "string" ? msg.error : undefined,
      data: msg.data,
    });
  }

  private observeToolCall(msg: Record<string, unknown>): void {
    if (msg.toolName !== TRANSIT_TOOL_NAME) return;
    const target = parseTarget(msg.args);
    if (target) this.pendingTarget = target;
    else this.hooks.warn?.(`pi-agent: ${TRANSIT_TOOL_NAME} call had no usable target (${JSON.stringify(msg.args)})`);
  }

  private mirrorMessage(message: unknown): void {
    if (message === null || typeof message !== "object") return;
    const m = message as Record<string, unknown>;
    const role = m.role;
    if (role === "user" || role === "assistant" || role === "toolResult") {
      this.hooks.onMessage(m as unknown as TranscriptMessage);
    } else if (typeof m.customType === "string") {
      this.hooks.onCustom(m.customType, m);
    }
  }

  private replyToExtensionUi(msg: Record<string, unknown>): void {
    const id = typeof msg.id === "string" ? msg.id : "";
    const method = typeof msg.method === "string" ? msg.method : "";
    if (id === "" || FIRE_AND_FORGET.has(method)) return;
    this.writeRaw({ type: "extension_ui_response", id, cancelled: true });
  }

  // -- internals -----------------------------------------------------------

  private emitTurn(r: TurnEnd): void {
    if (this.turnResolve) {
      const fn = this.turnResolve;
      this.turnResolve = null;
      fn(r);
    } else {
      this.turnQueue.push(r);
    }
  }

  private onAbort(): void {
    this.aborted = true;
    this.emitTurn("aborted");
  }

  private request(cmd: Record<string, unknown>): Promise<{ success: boolean; error?: string; data?: unknown }> {
    const id = `d${++this.nextId}`;
    return new Promise((resolve) => {
      if (this.exited) {
        resolve({ success: false, error: "pi exited" });
        return;
      }
      this.pending.set(id, { resolve });
      this.writeRaw({ id, ...cmd });
    });
  }

  private writeRaw(obj: Record<string, unknown>): void {
    const bytes = new TextEncoder().encode(JSON.stringify(obj) + "\n");
    void this.child.stdin.write(bytes).catch((e) => this.hooks.warn?.(`pi-agent: stdin write failed (${errMsg(e)})`));
  }
}

/**
 * Maps the `autosk_transit` tool arguments to a {@link StepTarget}. Accepts the
 * primary `{ to: "<step>|done|cancel|human" }` shape plus the explicit
 * `{ step }` / `{ status }` shapes for robustness.
 */
export function parseTarget(args: unknown): StepTarget | null {
  if (args === null || typeof args !== "object") return null;
  const a = args as Record<string, unknown>;
  if (typeof a.to === "string" && a.to.trim() !== "") {
    const to = a.to.trim();
    if (to === "done" || to === "cancel" || to === "human") return { status: to };
    return { step: to };
  }
  if (typeof a.step === "string" && a.step.trim() !== "") return { step: a.step.trim() };
  if (a.status === "done" || a.status === "cancel" || a.status === "human") return { status: a.status };
  return null;
}

/**
 * Builds the pi input command for a steer/followup given pi's streaming state
 * (port of v1 `build_input_command`, crates/autoskd/src/server.rs). While idle
 * pi only takes a fresh `prompt`; while streaming a steer maps to `steer` and a
 * followup to `follow_up` — the real pi command TYPES, not a field on `prompt`.
 */
export function buildInputCommand(
  kind: "steer" | "followup",
  message: string,
  streaming: boolean,
): { cmd: Record<string, unknown>; label: string } {
  if (!streaming) return { cmd: { type: "prompt", message }, label: "prompt" };
  if (kind === "followup") return { cmd: { type: "follow_up", message }, label: "follow_up" };
  return { cmd: { type: "steer", message }, label: "steer" };
}

/**
 * Conservative state-mismatch detector (port of v1 `is_state_mismatch`). A
 * `true` here means pi rejected an input command because our streaming view
 * raced its own — the cue to retry with the opposite dispatch shape.
 */
export function isStateMismatch(error: string | undefined): boolean {
  if (!error || error === "") return false;
  const lower = error.toLowerCase();
  const tokens = [
    "not streaming",
    "already streaming",
    "no run",
    "no active run",
    "no_active_run",
    "idle",
    "in_progress",
    "state mismatch",
    "state_mismatch",
  ];
  return tokens.some((t) => lower.includes(t));
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/**
 * Matches ANSI / terminal control sequences:
 *  - CSI: `ESC [` + parameter bytes (`0x30-0x3f`, covers `?`/`<`/`>`/`=`/digits/`;`)
 *    + intermediate bytes (`0x20-0x2f`) + a final byte (`0x40-0x7e`);
 *  - OSC: `ESC ]` … terminated by BEL (`0x07`) or ST (`ESC \`);
 *  - bare two-byte escapes: `ESC` + a single byte in `0x40-0x5f`.
 * pi emits a teardown burst of these (mouse tracking off, leave alt-screen, end
 * synchronized output) on exit; the example `\u001b[?2026h…\u001b[?2026l` is all CSI.
 */
const ANSI_CONTROL =
  // eslint-disable-next-line no-control-regex
  /\u001b\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)|\u001b[@-_]/g;

/** Removes ANSI/terminal control sequences from a (stderr) line. */
export function stripAnsi(s: string): string {
  return s.replace(ANSI_CONTROL, "");
}
