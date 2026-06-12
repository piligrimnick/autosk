/**
 * The per-project extension registry (plan §3.6, §3.7(1)).
 *
 * Each opened project gets its own registry, populated by running the union of
 * global + project-local extension factories (see `loader.ts`). It holds the
 * registered workflows + agents (code, not data), the load diagnostics, and the
 * rendered views the RPC layer (P5) serves over `registry.workflow.*` /
 * `registry.agent.list` and `project.diagnostics`.
 *
 * Two design rules from the plan:
 *  - **Error isolation.** A name collision or an invalid definition is never
 *    fatal: it is recorded as a diagnostic and the offending registration is
 *    skipped (first-registered wins, so the higher-priority source — project
 *    beats global — keeps the name). The factory keeps running; the rest of the
 *    registry stays usable (pi's `ExtensionRunner.onError` model).
 *  - **`singleStep` builtin.** The v1 `single:<agent>` synthetic workflows are a
 *    built-in factory the engine materialises on demand from `task.enroll
 *    {agent}` (`singleStepFor` / `resolveWorkflow`). They are never persisted
 *    and never appear in `listWorkflows()`. The `single:` prefix is **reserved**:
 *    `addWorkflow` rejects any extension that tries to register a `single:*`
 *    name (recorded as a diagnostic), so the builtin is the single source of
 *    truth for these workflows — no resolve/enroll drift, no list-invariant leak.
 */

import {
  singleStep,
  type AgentDefinition,
  type AgentInfo,
  type ExtensionLoadError,
  type StepTarget,
  type WorkflowDefinition,
  type WorkflowInfo,
  type WorkflowStepInfo,
} from "@autosk/sdk";

/** The name prefix the `singleStep` builtin materialises (`single:<agent>`). */
export const SINGLE_STEP_PREFIX = "single:";

/**
 * If `name` is a `single:<agent>` synthetic-workflow name, returns the agent
 * part; otherwise `null`. The agent part must be non-empty.
 */
export function parseSingleStepName(name: string): string | null {
  if (!name.startsWith(SINGLE_STEP_PREFIX)) return null;
  const agent = name.slice(SINGLE_STEP_PREFIX.length);
  return agent.length > 0 ? agent : null;
}

/** The result of validating a `(workflow, step)` position against the registry. */
export interface PositionValidation {
  ok: boolean;
  /** Present (with the `workflow_missing:` prefix) when `ok` is false. */
  error?: string;
}

export class ExtensionRegistry {
  private readonly workflows = new Map<string, WorkflowDefinition>();
  private readonly agents = new Map<string, AgentDefinition>();
  private readonly loadErrors: ExtensionLoadError[] = [];

  // -- diagnostics ---------------------------------------------------------

  /** The accumulated load diagnostics (a fresh copy). */
  get diagnostics(): ExtensionLoadError[] {
    return this.loadErrors.map((e) => ({ ...e }));
  }

  /** Records one load diagnostic (an extension that failed or collided). */
  recordDiagnostic(source: string, error: string): void {
    this.loadErrors.push({ source, error });
  }

  // -- registration (called by the per-extension AutoskAPI handle) ---------

  /**
   * Registers a workflow on behalf of `source`. A duplicate name, an empty
   * name, a reserved `single:*` name, an absent `steps`, or a `firstStep` that
   * is not a declared step are each recorded as a diagnostic and skip the
   * registration (never throw — the factory keeps running). First-registered
   * wins on a collision.
   */
  addWorkflow(source: string, workflow: WorkflowDefinition): void {
    const name = workflow?.name;
    if (typeof name !== "string" || name.length === 0) {
      this.recordDiagnostic(source, "registerWorkflow: workflow.name must be a non-empty string");
      return;
    }
    if (name.startsWith(SINGLE_STEP_PREFIX)) {
      // The `single:` namespace belongs to the `singleStep` builtin alone
      // (materialised on demand, never registered). Letting an extension claim
      // a `single:*` name would (a) leak a synthetic into listWorkflows() and
      // (b) diverge resolveWorkflow (map hit) from singleStepFor (always the
      // builtin) for the same enrolled task — so reject it outright.
      this.recordDiagnostic(
        source,
        `registerWorkflow: ${JSON.stringify(name)} uses the reserved "${SINGLE_STEP_PREFIX}*" synthetic-workflow namespace`,
      );
      return;
    }
    if (this.workflows.has(name)) {
      this.recordDiagnostic(source, `duplicate workflow name: ${name}`);
      return;
    }
    if (!workflow.steps || typeof workflow.steps !== "object") {
      this.recordDiagnostic(source, `registerWorkflow: "${name}" has no steps`);
      return;
    }
    if (!(workflow.firstStep in workflow.steps)) {
      this.recordDiagnostic(
        source,
        `registerWorkflow: "${name}" firstStep "${workflow.firstStep}" is not a declared step`,
      );
      return;
    }
    this.workflows.set(name, workflow);
  }

  /**
   * Registers an agent on behalf of `source`. A duplicate or empty name is
   * recorded as a diagnostic and skipped (never throws). First wins.
   */
  addAgent(source: string, agent: AgentDefinition): void {
    const name = agent?.name;
    if (typeof name !== "string" || name.length === 0) {
      this.recordDiagnostic(source, "registerAgent: agent.name must be a non-empty string");
      return;
    }
    if (this.agents.has(name)) {
      this.recordDiagnostic(source, `duplicate agent name: ${name}`);
      return;
    }
    this.agents.set(name, agent);
  }

  // -- resolution (engine-facing) ------------------------------------------

  /** The registered agent definition, if any. */
  getAgent(name: string): AgentDefinition | undefined {
    return this.agents.get(name);
  }

  /** Whether an agent is registered. */
  hasAgent(name: string): boolean {
    return this.agents.has(name);
  }

  /**
   * Resolves a workflow by name. A registered (non-synthetic) workflow wins;
   * a `single:<agent>` name can never be registered (the prefix is reserved in
   * {@link addWorkflow}), so it always materialises the `singleStep` builtin on
   * demand — IFF the agent is registered. Unknown ⇒ `undefined`. Keeping the
   * builtin the sole source of truth means `resolveWorkflow` and
   * {@link singleStepFor} can never disagree about a `single:*` workflow.
   */
  resolveWorkflow(name: string): WorkflowDefinition | undefined {
    const direct = this.workflows.get(name);
    if (direct) return direct;
    const agentName = parseSingleStepName(name);
    if (agentName !== null && this.agents.has(agentName)) {
      return singleStep(agentName);
    }
    return undefined;
  }

  /**
   * Materialises the `single:<agent>` workflow for `task.enroll {agent}`. Throws
   * if the agent is not registered. The result is never persisted and never
   * appears in {@link listWorkflows}.
   */
  singleStepFor(agentName: string): WorkflowDefinition {
    if (!this.agents.has(agentName)) {
      throw new Error(`cannot enroll: unknown agent ${JSON.stringify(agentName)}`);
    }
    return singleStep(agentName);
  }

  // -- live-code hazard validation -----------------------------------------

  /**
   * Validates an in-flight task's `(workflow, step)` against the registry
   * (plan §3.6). An unknown workflow OR a step missing from a known workflow
   * both yield `{ ok:false, error:"workflow_missing: …" }`.
   */
  validatePosition(workflow: string, step: string): PositionValidation {
    const wf = this.resolveWorkflow(workflow);
    if (!wf) return { ok: false, error: `workflow_missing: ${workflow}` };
    if (!(step in wf.steps)) {
      return { ok: false, error: `workflow_missing: ${workflow} has no step ${step}` };
    }
    return { ok: true };
  }

  // -- rendered views (proto-v2) -------------------------------------------

  /** Registered workflows rendered for `registry.workflow.list`, sorted by name. */
  listWorkflows(): WorkflowInfo[] {
    return [...this.workflows.values()]
      .map(renderWorkflowInfo)
      .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  }

  /**
   * A single workflow rendered for `registry.workflow.get`. Resolves
   * `single:<agent>` synthetics too (so a client can inspect an enrolled task's
   * workflow) — they still never appear in {@link listWorkflows}.
   */
  getWorkflowInfo(name: string): WorkflowInfo | undefined {
    const wf = this.resolveWorkflow(name);
    return wf ? renderWorkflowInfo(wf) : undefined;
  }

  /** Registered agents rendered for `registry.agent.list`, sorted by name. */
  listAgents(): AgentInfo[] {
    return [...this.agents.keys()].sort().map((name) => ({ name }));
  }

  /** Registered workflow names (sorted) — the deterministic merge result. */
  workflowNames(): string[] {
    return [...this.workflows.keys()].sort();
  }

  /** Registered agent names (sorted). */
  agentNames(): string[] {
    return [...this.agents.keys()].sort();
  }
}

/**
 * Renders a {@link WorkflowDefinition} (code) to its {@link WorkflowInfo} wire
 * projection (plan §4 `registry.workflow.get`).
 *
 * Per-step `targets` is the **conservative declared set** (a superset): because
 * the real graph is decided at runtime by `onTransit`, the engine cannot know
 * the exact edges, so it declares every step in the workflow — INCLUDING the
 * step itself, since a self-loop (re-run the same agent) is a structurally valid
 * retry target — plus the three terminal/park statuses. Step names are sorted so
 * the projection is byte-deterministic.
 */
export function renderWorkflowInfo(wf: WorkflowDefinition): WorkflowInfo {
  const stepNames = Object.keys(wf.steps).sort();
  const statusTargets: StepTarget[] = [
    { status: "done" },
    { status: "cancel" },
    { status: "human" },
  ];
  // Every step (the self-loop included) is a declared target — the projection is
  // a superset of the runtime graph, not the sibling-only subset.
  const stepTargets: StepTarget[] = stepNames.map((s) => ({ step: s }));
  const steps: WorkflowStepInfo[] = stepNames.map((stepName) => {
    const def = wf.steps[stepName]!;
    return {
      name: stepName,
      agent: def.agent ?? null,
      human: def.human ?? false,
      targets: [...stepTargets, ...statusTargets],
    };
  });
  const info: WorkflowInfo = {
    name: wf.name,
    first_step: wf.firstStep,
    steps,
    isolation: wf.isolation?.tag ?? "none",
  };
  if (wf.description !== undefined) info.description = wf.description;
  return info;
}
