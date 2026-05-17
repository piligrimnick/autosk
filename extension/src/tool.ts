import type {
  ExtensionAPI,
  ExtensionContext,
} from "@earendil-works/pi-coding-agent";
import { runAction } from "./actions.ts";
import { renderAutoskCall, renderAutoskResult } from "./render.ts";
import {
  AutoskParamsSchema,
  type AutoskArgs,
  type AutoskParams,
} from "./schema.ts";

const TOOL_NAME = "autosk";
const TOOL_LABEL = "autosk";

const TOOL_DESCRIPTION = `Drive the local autosk task tracker (./.autosk/db) for durable, agent-friendly task lists.
Use this for ready → claim → done loops, decomposition with blockers, and recording follow-up work.
Never edit ./.autosk/db directly; route every change through this tool. Prefer this tool over shelling out to \`autosk\` via bash.

Params: { action: <enum>, args?: { ... } }. Args keys are reused across actions; each action ignores keys it does not need. All ids look like "as-XXXX".

Actions:
- ready    args.limit?       — list new + unblocked tasks, priority-sorted. Pick the first id to claim next.
- next                       — alias for ready --limit 1.
- list     args.status_filter? args.priority_filter? args.limit?
                              — broader listing; default status filter is "new,claimed"; use status_filter:"all" for everything.
- show     args.id            — full task incl. blocked_by / blocks / blocked.
- create   args.title (req), args.description?, args.priority? (0..3, default 2),
           args.blocks? (ids this new task will block), args.blocked_by? (ids that block this new task).
- update   args.id (req), any of: title, description, priority, status. Any status → any status.
- claim    args.id            — idempotent: new|claimed → claimed.
- done     args.id
- cancel   args.id
- reopen   args.id            — done|cancelled → new.
- block    args.id, args.blocker_ids[]  — each blocker_id blocks args.id.
- unblock  args.id, args.blocker_ids[] OR args.all:true.
- dep_list args.id            — returns the task with its blocked_by / blocks lists.
- init     args.prefix?       — explicit DB init (write commands auto-init otherwise).

Result shape:
- details.kind "task"   → single task object (id, title, description, status, priority, blocked, blocked_by, blocks, timestamps).
- details.kind "tasks"  → array (list/ready/next).
- details.kind "ack"    → init only.
- details.kind "error"  → { reason: "missing_binary"|"invalid_args"|"cli_error"|"parse_error"|"aborted", message, ... }.

Operates relative to the agent's cwd; autosk walks up to find .autosk/db (or auto-creates one on first write).`;

const TOOL_PROMPT_SNIPPET =
  "Manage durable task lists in ./.autosk/db: create/claim/done/block/unblock/list/ready/show via a single autosk tool.";

const TOOL_PROMPT_GUIDELINES = [
  "Use `autosk` for any tasks mutation.",
  "Before starting work, call `autosk` with action:'ready' and claim the top id rather than guessing.",
  "`claim` is idempotent — re-calling on an already-claimed task is the canonical way to record 'I am working on this'.",
  "When you discover a blocker mid-task, create a new blocker task, then call action:'block' on the task you claimed; never inline blockers as prose.",
  "Decompose oversized work by creating subtasks with `--blocks` so the parent disappears from `ready` until subtasks finish.",
  "Close with action:'done' for completed work and action:'cancel' for abandoned work; do not delete tasks.",
] as const;

export function registerAutoskTool(pi: ExtensionAPI) {
  pi.registerTool({
    name: TOOL_NAME,
    label: TOOL_LABEL,
    description: TOOL_DESCRIPTION,
    promptSnippet: TOOL_PROMPT_SNIPPET,
    promptGuidelines: [...TOOL_PROMPT_GUIDELINES],
    parameters: AutoskParamsSchema,
    execute: (toolCallId, params, signal, _onUpdate, ctx) =>
      executeAutoskTool(pi, toolCallId, params as AutoskParams, signal, ctx),
    renderCall: renderAutoskCall,
    renderResult: renderAutoskResult,
  });
}

async function executeAutoskTool(
  pi: Pick<ExtensionAPI, "exec">,
  _toolCallId: string,
  params: AutoskParams,
  signal: AbortSignal | undefined,
  ctx: ExtensionContext,
) {
  const args: AutoskArgs = params.args ?? {};
  const response = await runAction(pi, params.action, args, {
    cwd: ctx.cwd,
    signal,
  });
  const isError = response.details.kind === "error";
  return {
    content: [{ type: "text" as const, text: response.text }],
    details: response.details,
    isError,
  };
}
