/**
 * `@autosk/claude-agent` — drive Claude Code (`claude -p` headless stream-json)
 * as an autoskd v2 agent. The structural twin of `@autosk/pi-agent`, with Claude
 * Code as the harness instead of `pi --mode rpc`.
 *
 * `claudeAgent({...})` returns an {@link AgentDefinition} the engine runs for a
 * workflow step: spawn `claude -p --output-format stream-json --input-format
 * stream-json`, drive it over NDJSON stdio via {@link ClaudeDriver}, mirror
 * assistant/tool messages 1:1 into the autosk transcript, and run the
 * kickback/corrections loop to a single transition.
 *
 * Tool surface (plan §5): one stdio `autosk` MCP server (`autoskd mcp`) registered
 * via `--mcp-config`, exposing `transit` (ack-only, observed by the driver),
 * `task`, and `comment` (executed for real by the MCP server shelling out to
 * `autosk --json`). The model sees `mcp__autosk__transit` / `mcp__autosk__task` /
 * `mcp__autosk__comment`.
 */

import { readFile } from "node:fs/promises";
import { join } from "node:path";

import type { AgentDefinition, AgentRunContext, AutoskAPI, StepTarget } from "@autosk/sdk";

import { ClaudeDriver, TRANSIT_TOOL_NAME } from "./driver.ts";
import { kickbackMessage, renderInitialPrompt, rejectionMessage } from "./prompt.ts";

export {
  ClaudeDriver,
  Coalescer,
  parseTarget,
  stripAnsi,
  TRANSIT_TOOL_NAME,
  type TurnEnd,
  type ClaudeDriverHooks,
} from "./driver.ts";
export {
  mapAssistant,
  mapUser,
  mapResultUsage,
  mapStopReason,
  mapUsage,
  type ClaudeAssistantMessage,
} from "./wire.ts";
export { renderInitialPrompt, kickbackMessage, rejectionMessage, targetLabel, targetLabels } from "./prompt.ts";

/** The full MCP tool names Claude sees, used for the headless allowlist. */
const TASK_TOOL_NAME = "mcp__autosk__task";
const COMMENT_TOOL_NAME = "mcp__autosk__comment";

/**
 * Configuration for one Claude-Code-backed agent role. No `name`: a claude agent
 * is an inline step value, so its display name is the workflow step key (taken
 * from `ctx.workflows.current.step` at run time).
 */
export interface ClaudeAgentOptions {
  /** Claude model alias/name, e.g. `"sonnet"` / `"opus"` (`--model`). */
  model?: string;
  /** Inline first-message seed (wins over {@link firstMessageFile}). */
  firstMessage?: string;
  /** Path to a file whose contents seed the first message. */
  firstMessageFile?: string;
  /** Role guidance appended to the system prompt (`--append-system-prompt`). */
  appendSystemPrompt?: string;
  /**
   * Permission mode for an unattended run (`--permission-mode`). Defaults to
   * `"acceptEdits"`. A headless run that hits an un-allowed tool ABORTS (it
   * cannot prompt), so the posture must be non-interactive-safe.
   */
  permissionMode?: "default" | "acceptEdits" | "plan" | "dontAsk" | "bypassPermissions";
  /**
   * Skip all permission prompts (`--dangerously-skip-permissions`); wins over
   * {@link permissionMode}. Intended for runs under worktree isolation (the
   * worktree is the sandbox).
   */
  dangerouslySkipPermissions?: boolean;
  /** Auto-approved tools (`--allowedTools`, csv). The autosk MCP tools are added automatically. */
  allowedTools?: string[];
  /** Denied tools (`--disallowedTools`, csv). */
  disallowedTools?: string[];
  /** `--bare` for hermetic runs (skip project CLAUDE.md / .mcp.json / hooks discovery). */
  bare?: boolean;
  /**
   * Register the `autosk` MCP server (transit + task + comment). Default `true`.
   * `false` omits `--mcp-config` (no autosk tools; transit then never fires → the
   * run always parks).
   */
  autoskTools?: boolean;
  /** Extra args forwarded verbatim to `claude`. */
  extraArgs?: string[];
  /**
   * Max corrective turns before giving up and returning without a transit (the
   * engine then parks via `agent_did_not_transit`). Default `3`.
   */
  maxCorrections?: number;
  /** `claude` binary to spawn. Defaults to `$AUTOSK_CLAUDE_BIN` or `"claude"`. The e2e tests point this at a stub. */
  claudeBin?: string;
}

/** Per-session driver registry so steer/followup/abort reach the live claude. */
const liveSessions = new Map<string, ClaudeDriver>();

/**
 * Builds the Claude-Code-backed {@link AgentDefinition} for one role. The
 * returned agent spawns `claude -p` stream-json on each `onRun`, drives it to a
 * single `mcp__autosk__transit`, and forwards steer/followup/abort into the live
 * process.
 */
export function claudeAgent(opts: ClaudeAgentOptions = {}): AgentDefinition {
  const maxCorrections = opts.maxCorrections ?? 3;
  // Resolve the first-message seed at most once per process (the loader caches
  // extension modules, so re-reading the prompt file every onRun is wasted IO).
  let firstMessageOnce: Promise<string> | null = null;
  const firstMessage = (): Promise<string> => (firstMessageOnce ??= resolveFirstMessage(opts));

  return {
    async onRun(ctx: AgentRunContext): Promise<void> {
      // Interactive (taskless) chat: a separate loop with no transit (plan §7).
      if (ctx.mode === "interactive") {
        await runChat(ctx, opts);
        return;
      }
      const mcpConfig = opts.autoskTools === false ? undefined : buildMcpConfig(ctx, { transit: true });
      const cmd = buildClaudeCommand(opts, { mcpConfig });
      // Spawn claude in the run directory (the worktree under isolation), but
      // tell any `autosk` CLI it invokes — directly or via the autosk MCP tools —
      // which project to target (AUTOSK_CWD), who to attribute comments to
      // (AUTOSK_AGENT), and which running daemon to join (AUTOSK_SOCK).
      const child = ctx.spawn(cmd, { cwd: ctx.cwd, env: autoskEnv(ctx) });
      const driver = new ClaudeDriver(child, {
        onMessage: (m) => ctx.log.message(m),
        onCustom: (t, d) => ctx.log.custom(t, d),
        onPartial: (m) => ctx.partial(m),
        signal: ctx.signal,
        warn: (message) => ctx.log.custom("claude-agent:warn", { message }),
      });
      // Register the live driver BEFORE the first `await`: the engine marks the
      // session steerable as soon as onRun starts, so a steer/followup landing
      // while we resolve the first message must reach this driver.
      liveSessions.set(ctx.session.id, driver);
      try {
        const seed = await firstMessage();
        await runTurns(ctx, driver, seed, maxCorrections);
      } finally {
        liveSessions.delete(ctx.session.id);
        await driver.shutdown().catch(() => {});
      }
    },

    onSteer: (ctx, message) => forward(ctx, "steer", message),
    onFollowup: (ctx, message) => forward(ctx, "followup", message),

    async onAbort(ctx): Promise<void> {
      const driver = liveSessions.get(ctx.session.id);
      // ctx.signal has already fired (the engine aborts before onAbort), so the
      // child is being killed; this just asks claude to wind down gracefully.
      if (driver) await driver.shutdown().catch(() => {});
    },
  };
}

/**
 * The kickback/corrections turn loop (ported from pi-agent): prompt → wait for
 * the turn to end → if the model called the transit tool, commit it (a rejection
 * is fed back as a correction); else kick back. After the budget is exhausted,
 * return WITHOUT a transit so the engine parks the task (`agent_did_not_transit`).
 */
async function runTurns(
  ctx: AgentRunContext,
  driver: ClaudeDriver,
  firstMessage: string,
  maxCorrections: number,
): Promise<void> {
  const targets = ctx.workflows.current.targets;
  const task = await ctx.tasks.current();
  const comments = await ctx.tasks.comments();
  await driver.sendPrompt(
    renderInitialPrompt({
      firstMessage,
      agentName: ctx.workflows.current.step,
      workflow: ctx.workflows.current.workflow,
      step: ctx.workflows.current.step,
      task,
      targets,
      comments,
    }),
  );

  let corrections = 0;
  for (;;) {
    const how = await driver.waitForTurnEnd();
    if (how === "aborted") return; // engine finalises the session as aborted
    if (how === "exited") {
      throw new Error(`claude exited (code=${driver.exitCode}) before recording a transition`);
    }

    const target = driver.takePendingTarget();
    if (target) {
      const rejection = await commitTransit(ctx, target);
      if (!rejection) return; // success — the engine sealed the session as done
      if (corrections >= maxCorrections) return; // give up → engine parks
      corrections++;
      await driver.sendPrompt(rejectionMessage(target, rejection, targets, corrections, maxCorrections));
      continue;
    }

    // No transit this turn → kick back (bounded by the same budget).
    if (corrections >= maxCorrections) return; // give up → engine parks
    corrections++;
    await driver.sendPrompt(kickbackMessage(task.id, targets, corrections, maxCorrections));
  }
}

/**
 * The interactive (taskless) chat loop (plan §7): spawn `claude -p` stream-json
 * with the `autosk` MCP server but WITHOUT transit (it has nothing to transit in
 * a chat), send NO initial prompt (the session opens idle), register the driver
 * before the first `await`, wire `onActivity` → `ctx.setActivity`, then wait for
 * `ctx.signal`. Each composer message arrives via `onFollowup` →
 * `driver.input("followup", msg)` (idle → a fresh turn).
 */
async function runChat(ctx: AgentRunContext, opts: ClaudeAgentOptions): Promise<void> {
  const mcpConfig = opts.autoskTools === false ? undefined : buildMcpConfig(ctx, { transit: false });
  const cmd = buildClaudeCommand(opts, { mcpConfig, interactive: true });
  const child = ctx.spawn(cmd, { cwd: ctx.cwd, env: autoskEnv(ctx) });
  const driver = new ClaudeDriver(child, {
    onMessage: (m) => ctx.log.message(m),
    onCustom: (t, d) => ctx.log.custom(t, d),
    onPartial: (m) => ctx.partial(m),
    signal: ctx.signal,
    onActivity: (busy) => ctx.setActivity(busy ? "busy" : "idle"),
    warn: (message) => ctx.log.custom("claude-agent:warn", { message }),
  });
  // Register the live driver BEFORE the first `await` so the very first composer
  // message (delivered via onFollowup) reaches this driver and starts a turn.
  liveSessions.set(ctx.session.id, driver);
  try {
    // No initial prompt: an interactive session is empty until the user types.
    await waitForSignal(ctx.signal);
  } finally {
    liveSessions.delete(ctx.session.id);
    await driver.shutdown().catch(() => {});
  }
}

/** Resolves when the abort signal fires (the user ended or aborted the session). */
function waitForSignal(signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise<void>((resolve) => {
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

/** Commits a transit; returns `null` on success or the rejection message string. */
async function commitTransit(ctx: AgentRunContext, target: StepTarget): Promise<string | null> {
  try {
    await ctx.transit(target);
    return null;
  } catch (e) {
    return e instanceof Error ? e.message : String(e);
  }
}

/** Forwards a steer/followup into the live claude for the calling session. */
async function forward(ctx: AgentRunContext, kind: "steer" | "followup", message: string): Promise<void> {
  const driver = liveSessions.get(ctx.session.id);
  if (driver) await driver.input(kind, message);
}

/** Resolves the role's first-message seed (inline wins, else file, else ""). */
async function resolveFirstMessage(opts: ClaudeAgentOptions): Promise<string> {
  if (opts.firstMessage !== undefined) return opts.firstMessage;
  if (opts.firstMessageFile) return readFile(opts.firstMessageFile, "utf8");
  return "";
}

/**
 * The autosk env handed to the spawned claude so any `autosk` CLI it runs (and
 * the `autoskd mcp` server's shell-out) resolves the ORIGINAL project (not the
 * worktree it runs in), attributes comments to the running step, and joins the
 * SAME running daemon (no second auto-spawn). `ctx.spawn` merges this over
 * `process.env`, so PATH/HOME and the rest are preserved.
 */
export function autoskEnv(ctx: AgentRunContext): Record<string, string> {
  const env: Record<string, string> = {
    AUTOSK_CWD: ctx.projectRoot,
    AUTOSK_AGENT: ctx.workflows.current.step,
  };
  const sock = resolveSock();
  if (sock) env.AUTOSK_SOCK = sock;
  return env;
}

/** Resolves the running daemon's socket: `$AUTOSK_SOCK` → `~/.autosk/daemon.sock`. */
function resolveSock(): string {
  const env = process.env.AUTOSK_SOCK;
  if (env && env.length > 0) return env;
  const home = process.env.HOME;
  return home && home.length > 0 ? join(home, ".autosk", "daemon.sock") : "";
}

/** The autoskd binary to re-invoke for `autoskd mcp`: `$AUTOSKD_BIN` (dev) → `process.execPath` (compiled). */
export function resolveAutoskdBin(): string {
  const env = process.env.AUTOSKD_BIN;
  return env && env.length > 0 ? env : process.execPath;
}

/**
 * Builds the inline `--mcp-config` JSON pointing Claude at `autoskd mcp`, with
 * the autosk env (AUTOSK_CWD/AUTOSK_AGENT/AUTOSK_SOCK) baked in so the server's
 * `autosk` shell-out targets the right project + running daemon. `transit:true`
 * sets `AUTOSK_MCP_TRANSIT=1` so the server also advertises the transit tool
 * (task mode only).
 */
export function buildMcpConfig(ctx: AgentRunContext, flags: { transit: boolean }): string {
  const env: Record<string, string> = {
    AUTOSK_CWD: ctx.projectRoot,
    AUTOSK_AGENT: ctx.workflows.current.step,
  };
  const sock = resolveSock();
  if (sock) env.AUTOSK_SOCK = sock;
  if (flags.transit) env.AUTOSK_MCP_TRANSIT = "1";
  return JSON.stringify({
    mcpServers: {
      autosk: {
        type: "stdio",
        command: resolveAutoskdBin(),
        args: ["mcp"],
        env,
      },
    },
  });
}

/**
 * Builds the `claude -p … stream-json` argv. In interactive (chat) mode the
 * transit tool is not registered (the MCP config is built with `transit:false`),
 * matching pi-agent skipping the transit tool in a chat.
 */
export function buildClaudeCommand(
  opts: ClaudeAgentOptions,
  flags: { mcpConfig?: string; interactive?: boolean } = {},
): string[] {
  const bin = opts.claudeBin ?? process.env.AUTOSK_CLAUDE_BIN ?? "claude";
  const args = [
    bin,
    "-p",
    "--output-format",
    "stream-json",
    "--input-format",
    "stream-json",
    "--verbose",
    "--include-partial-messages",
    "--replay-user-messages",
  ];
  if (flags.mcpConfig !== undefined) {
    args.push("--mcp-config", flags.mcpConfig);
    // Pre-approve the autosk MCP tools so a headless run never silently aborts on
    // a permission prompt for transit/task/comment.
    const autoskTools = flags.interactive
      ? [TASK_TOOL_NAME, COMMENT_TOOL_NAME]
      : [TRANSIT_TOOL_NAME, TASK_TOOL_NAME, COMMENT_TOOL_NAME];
    opts = { ...opts, allowedTools: [...autoskTools, ...(opts.allowedTools ?? [])] };
  }
  if (opts.model) args.push("--model", opts.model);
  if (opts.dangerouslySkipPermissions) {
    args.push("--dangerously-skip-permissions");
  } else {
    args.push("--permission-mode", opts.permissionMode ?? "acceptEdits");
  }
  if (opts.allowedTools && opts.allowedTools.length > 0) args.push("--allowedTools", opts.allowedTools.join(","));
  if (opts.disallowedTools && opts.disallowedTools.length > 0) {
    args.push("--disallowedTools", opts.disallowedTools.join(","));
  }
  if (opts.appendSystemPrompt) args.push("--append-system-prompt", opts.appendSystemPrompt);
  if (opts.bare) args.push("--bare");
  args.push(...(opts.extraArgs ?? []));
  return args;
}

/**
 * Default extension factory. Workflow roles are registered by a consuming
 * extension via `claudeAgent({...})` as inline step values. Here we register a
 * single NAMED agent so an interactive (taskless) chat session can be opened
 * against it (plan §7).
 */
export default function claudeAgentExtension(autosk: AutoskAPI): void {
  autosk.registerAgent({
    name: "@autosk/claude-agent",
    description: "system-wide Claude Code agent",
    agent: claudeAgent(), // default options (model from Claude's own defaults)
  });
}
