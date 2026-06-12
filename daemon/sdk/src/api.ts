/**
 * The `AutoskAPI` an extension factory receives (plan §3.6).
 *
 * Extensions are default-export factories, mirroring pi:
 *
 * ```ts
 * import type { AutoskAPI } from "@autosk/sdk";
 * export default function (autosk: AutoskAPI) {
 *   autosk.registerAgent(...);
 *   autosk.registerWorkflow(...);
 * }
 * ```
 *
 * `registerWorkflow` / `registerAgent` write into the calling project's
 * registry. A name collision is a load error surfaced via
 * `project.diagnostics`; it never takes the daemon down.
 */

import type { AgentDefinition } from "./agent.ts";
import type { WorkflowDefinition } from "./workflow.ts";

export interface AutoskAPI {
  registerWorkflow(workflow: WorkflowDefinition): void;
  registerAgent(agent: AgentDefinition): void;
}

/** The default export an extension module must provide. */
export type ExtensionFactory = (autosk: AutoskAPI) => void | Promise<void>;
