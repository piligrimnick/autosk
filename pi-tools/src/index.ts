import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

import { registerCommentTool } from "./comment.ts";
import { registerTaskTool } from "./task.ts";

/**
 * pi extension entry point. Registers the autosk management tools an agent uses
 * to drive the local task tracker:
 *
 *   - `autosk_task`    — create / update / show / list
 *   - `autosk_comment` — add / list
 *
 * Both shell out to the local `autosk` CLI (must be on $PATH) and speak its
 * `--json` wire format. They resolve the project from the child's cwd, or from
 * `$AUTOSK_CWD` when set — the autosk agent runtime points that at the real
 * project root so the tools keep working inside an isolated worktree.
 *
 * Workflow transitions are NOT a tool here: an `@autosk/pi-agent`-driven step
 * records its transition through the in-process `autosk_transit` channel, so a
 * `step`/`transit` tool would be a no-op outside a workflow run.
 */
export default function autoskToolsExtension(pi: ExtensionAPI) {
	registerTaskTool(pi);
	registerCommentTool(pi);
}

export { registerTaskTool } from "./task.ts";
export { registerCommentTool } from "./comment.ts";
export * from "./types.ts";
