/**
 * The injected pi extension that gives pi the autosk tool surface over the
 * per-session HTTP MCP server (plan §5.2) — the thin-image / sandbox path.
 *
 * `@autosk/pi-agent` passes this file to the spawned `pi --mode rpc` via `-e`
 * WHEN a `sandbox` is configured. Unlike the off-docker `@autosk/pi-tools` path
 * (which shells out to `autosk … --json`), this registers `autosk_transit` /
 * `autosk_task` / `autosk_comment` as pi-tools that `fetch()` the host MCP server
 * minted by `ctx.newMCPServer()`: the agent injects its endpoint + bearer as env
 * (`AUTOSK_MCP_URL` / `AUTOSK_MCP_TOKEN`, the URL already rewritten to
 * `host.docker.internal` for a container). So the image needs neither `autosk`
 * nor a mounted daemon socket.
 *
 * `autosk_transit` is ack-only (the autosk-side {@link PiDriver} OBSERVES the
 * call on pi's event stream and drives the real `ctx.transit(...)`), exactly as
 * the off-docker `pi-transit-extension.ts` does. `autosk_task` / `autosk_comment`
 * POST a JSON-RPC `tools/call` to the MCP server, which runs them against the
 * daemon's own store and returns the same `{ content, structuredContent }` shape
 * the shell-out path does.
 *
 * NB: this module is loaded by **pi's** toolchain (it imports pi's `typebox` /
 * `ExtensionAPI`), NOT by the autosk daemon, which only ever passes its PATH to
 * `pi -e`. It is therefore excluded from the autosk workspace's `tsc` typecheck.
 */

// These resolve inside pi's environment when pi loads the extension.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

const TaskStatusSchema = Type.Union([
  Type.Literal("new"),
  Type.Literal("work"),
  Type.Literal("human"),
  Type.Literal("done"),
  Type.Literal("cancel"),
  Type.Literal("all"),
]);

const TaskParamsSchema = Type.Object({
  action: Type.Union([Type.Literal("create"), Type.Literal("update"), Type.Literal("show"), Type.Literal("list")]),
  args: Type.Optional(
    Type.Partial(
      Type.Object({
        id: Type.String({ description: "Target task id (e.g. ask-a1b2c3). Required for show and update." }),
        title: Type.String({ description: "Task title. Required by create; optional by update." }),
        description: Type.String({ description: "Task description. Optional (create / update)." }),
        blocks: Type.Array(Type.String(), {
          description: "create only: this new task will be a blocker for each of these ids.",
        }),
        blocked_by: Type.Array(Type.String(), {
          description: "create only: each of these ids will block this new task.",
        }),
        workflow: Type.String({
          description: "create only: enroll the new task into this named workflow at its first step.",
        }),
        statuses: Type.Array(TaskStatusSchema, {
          description: "list only: filter by status (or a single 'all' = no filter). Default: new, work, human.",
        }),
        limit: Type.Integer({ minimum: 0, description: "list only: max rows (0 = unlimited)." }),
      }),
    ),
  ),
});

const CommentParamsSchema = Type.Object({
  action: Type.Union([Type.Literal("add"), Type.Literal("list")]),
  args: Type.Optional(
    Type.Partial(
      Type.Object({
        task_id: Type.String({ description: "Target task id (e.g. ask-a1b2c3). Required for add and list." }),
        text: Type.String({ description: "Comment text. Required by add; non-empty." }),
        author: Type.String({ description: "Optional author name override. Defaults to the running step." }),
      }),
    ),
  ),
});

const TRANSIT_PARAMS = Type.Object({
  to: Type.String({
    description: "The transition target: a sibling step name, or one of done | cancel | human.",
  }),
});

const TASK_DESCRIPTION = `Manage autosk tasks (create / update / show / list) against the autosk daemon.
Use this tool for task reads and edits; prefer it over shelling out to \`autosk\` via bash.

Params: { action: "create"|"update"|"show"|"list", args: {...} }. Task ids look like "ask-XXXXXX".`;

const COMMENT_DESCRIPTION = `Append-only comments on autosk tasks. The workflow engine surfaces every prior comment at the top of each step's prompt, so this is the canonical cross-agent channel for a task.

Params: { action: "add"|"list", args: {...} }.`;

interface McpToolResult {
  content: { type: "text"; text: string }[];
  isError: boolean;
  structuredContent?: unknown;
}

/** POSTs a JSON-RPC `tools/call` to the per-session HTTP MCP server with the bearer. */
async function mcpCall(name: "task" | "comment", args: unknown, signal?: AbortSignal) {
  const url = process.env.AUTOSK_MCP_URL ?? "";
  const token = process.env.AUTOSK_MCP_TOKEN ?? "";
  if (url === "") return errResult(`autosk: AUTOSK_MCP_URL is not set — the ${name} tool cannot reach the daemon`);
  let body: { result?: McpToolResult; error?: { message?: string } };
  try {
    const res = await fetch(url, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
      body: JSON.stringify({ jsonrpc: "2.0", id: Date.now(), method: "tools/call", params: { name, arguments: args } }),
      signal,
    });
    if (!res.ok) return errResult(`autosk: MCP server returned HTTP ${res.status}`);
    body = (await res.json()) as { result?: McpToolResult; error?: { message?: string } };
  } catch (e) {
    return errResult(`autosk: MCP request failed (${e instanceof Error ? e.message : String(e)})`);
  }
  if (body.error) return errResult(`autosk: ${body.error.message ?? "MCP error"}`);
  const result = body.result;
  if (!result) return errResult("autosk: empty MCP response");
  return { content: result.content, details: result.structuredContent, isError: result.isError };
}

function errResult(text: string) {
  return { content: [{ type: "text" as const, text }], isError: true };
}

export default function autoskMcpExtension(pi: ExtensionAPI): void {
  // transit — ack only; the autosk-side driver observes the call and transits.
  pi.registerTool({
    name: "autosk_transit",
    label: "autosk transit",
    description:
      "Record the workflow transition for the current autosk task. Call this exactly once, when you " +
      "are done with the step, with `to` set to the chosen sibling step name or one of done | cancel | human. " +
      "If a transition is rejected you will be told and may call this again with a different target.",
    promptSnippet: "Record the chosen autosk workflow transition (call exactly once when done).",
    promptGuidelines: ["Call autosk_transit exactly once before you stop, with the chosen transition target."],
    parameters: TRANSIT_PARAMS,
    async execute(_toolCallId: unknown, params: unknown) {
      const to = String((params as { to: string }).to ?? "").trim();
      return {
        content: [
          {
            type: "text" as const,
            text:
              to === ""
                ? "autosk: no transition target provided — call autosk_transit again with a `to`."
                : `autosk: transition to "${to}" submitted.`,
          },
        ],
      };
    },
  });

  pi.registerTool({
    name: "autosk_task",
    label: "autosk task",
    description: TASK_DESCRIPTION,
    promptSnippet: "Create / update / show / list autosk tasks via the autosk daemon.",
    promptGuidelines: ["Use `autosk_task` for task reads/edits — prefer it over bashing the `autosk` CLI."],
    parameters: TaskParamsSchema,
    execute: (_callId: unknown, params: unknown, signal?: AbortSignal) => mcpCall("task", params, signal),
  });

  pi.registerTool({
    name: "autosk_comment",
    label: "autosk comment",
    description: COMMENT_DESCRIPTION,
    promptSnippet: "Append / read durable comments on autosk tasks via the autosk daemon.",
    promptGuidelines: ["Use `autosk_comment` to record progress notes, hand-offs, and follow-ups on a task."],
    parameters: CommentParamsSchema,
    execute: (_callId: unknown, params: unknown, signal?: AbortSignal) => mcpCall("comment", params, signal),
  });
}
