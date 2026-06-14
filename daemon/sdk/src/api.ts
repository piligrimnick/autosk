/**
 * The `AutoskAPI` an extension factory receives (plan §3.6).
 *
 * Extensions are default-export factories, mirroring pi:
 *
 * ```ts
 * import type { AutoskAPI } from "@autosk/sdk";
 * export default function (autosk: AutoskAPI) {
 *   autosk.registerWorkflow(...);
 * }
 * ```
 *
 * `registerWorkflow` writes into the calling project's registry. A workflow
 * registers its agents inline (each step value is an `AgentDefinition` or a
 * `statusStep`), so there is no separate agent registration. A name collision
 * or an invalid step shape is a load error surfaced via `project.diagnostics`;
 * it never takes the daemon down.
 */

import type { WorkflowDefinition } from "./workflow.ts";

export interface AutoskAPI {
  registerWorkflow(workflow: WorkflowDefinition): void;
}

/** The default export an extension module must provide. */
export type ExtensionFactory = (autosk: AutoskAPI) => void | Promise<void>;
