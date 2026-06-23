/**
 * The `AutoskAPI` an extension factory receives (plan §3.6).
 *
 * Extensions are default-export factories, mirroring pi:
 *
 * ```ts
 * import type { AutoskAPI } from "@autosk/sdk";
 * export default function (autosk: AutoskAPI) {
 *   autosk.registerWorkflow(...);
 *   autosk.registerAgent(...);
 * }
 * ```
 *
 * `registerWorkflow` writes into the calling project's registry. A workflow
 * registers its agents inline (each step value is an `AgentDefinition` or a
 * `statusStep`). `registerAgent` (plan §3.3) registers a NAMED agent that can
 * back an interactive (taskless) chat session — the agent picker lists every
 * registered agent. A name collision or an invalid shape is a load error
 * surfaced via `project.diagnostics`; it never takes the daemon down.
 */

import type { AgentDefinition } from "./agent.ts";
import type { WorkflowDefinition } from "./workflow.ts";

/**
 * A named agent registration (plan §3.3). The `agent` definition is run with its
 * default options for an interactive session; `name` is what the picker and
 * `session.create` reference.
 */
export interface AgentRegistration {
  name: string;
  description?: string;
  /** The agent definition used for interactive sessions (default options). */
  agent: AgentDefinition;
}

export interface AutoskAPI {
  registerWorkflow(workflow: WorkflowDefinition): void;
  registerAgent(registration: AgentRegistration): void;
}

/** The default export an extension module must provide. */
export type ExtensionFactory = (autosk: AutoskAPI) => void | Promise<void>;
