import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

import { registerCommentTool } from "./comment.ts";
import { registerStepTool } from "./step.ts";
import { registerTaskTool } from "./task.ts";

/**
 * pi extension entry point. Registers the three autosk tools that an agent
 * needs to drive the local task tracker:
 *
 *   - `autosk_task`    — create / update / show
 *   - `autosk_comment` — add / list
 *   - `autosk_step`    — next (record workflow transition for the active run)
 *
 * All three shell out to the local `autosk` CLI (must be on $PATH) and
 * speak its `--json` wire format.
 */
export default function autoskExtension(pi: ExtensionAPI) {
	registerTaskTool(pi);
	registerCommentTool(pi);
	registerStepTool(pi);
}
