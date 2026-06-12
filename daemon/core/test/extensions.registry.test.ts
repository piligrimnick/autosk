/**
 * ExtensionRegistry (plan §3.6): registration + error isolation, the
 * `singleStep` builtin, live-code validation, and the rendered proto-v2 views.
 *
 * These exercise the registry directly (no filesystem); the loader test wires
 * the same registry up to on-disk fixtures.
 */

import { describe, expect, test } from "bun:test";

import { ExtensionRegistry, parseSingleStepName, renderWorkflowInfo } from "../src/index.ts";
import type { AgentDefinition, WorkflowDefinition } from "@autosk/sdk";

const agent = (name: string): AgentDefinition => ({ name, async onRun() {} });

const wf = (name: string, extra: Partial<WorkflowDefinition> = {}): WorkflowDefinition => ({
  name,
  firstStep: "dev",
  steps: { dev: { agent: "a1" }, review: { agent: "a2" }, accept: { human: true } },
  ...extra,
});

describe("ExtensionRegistry registration", () => {
  test("registers workflows + agents and exposes sorted names", () => {
    const r = new ExtensionRegistry();
    r.addAgent("src", agent("a2"));
    r.addAgent("src", agent("a1"));
    r.addWorkflow("src", wf("beta"));
    r.addWorkflow("src", wf("alpha"));

    expect(r.workflowNames()).toEqual(["alpha", "beta"]);
    expect(r.agentNames()).toEqual(["a1", "a2"]);
    expect(r.diagnostics).toEqual([]);
  });

  test("a duplicate workflow name keeps the first and records a diagnostic", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("first", wf("alpha", { description: "the original" }));
    r.addWorkflow("second", wf("alpha", { description: "the loser" }));

    // First registration wins (higher priority source).
    expect(r.getWorkflowInfo("alpha")?.description).toBe("the original");
    expect(r.diagnostics).toEqual([{ source: "second", error: "duplicate workflow name: alpha" }]);
  });

  test("a duplicate agent name keeps the first and records a diagnostic", () => {
    const r = new ExtensionRegistry();
    r.addAgent("first", agent("a1"));
    r.addAgent("second", agent("a1"));
    expect(r.agentNames()).toEqual(["a1"]);
    expect(r.diagnostics).toEqual([{ source: "second", error: "duplicate agent name: a1" }]);
  });

  test("rejects an empty name and a firstStep that is not a declared step", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("s", { name: "", firstStep: "do", steps: { do: {} } });
    r.addWorkflow("s", { name: "bad", firstStep: "missing", steps: { do: {} } });
    r.addAgent("s", { name: "" } as AgentDefinition);

    expect(r.workflowNames()).toEqual([]);
    expect(r.agentNames()).toEqual([]);
    expect(r.diagnostics.map((d) => d.error)).toEqual([
      "registerWorkflow: workflow.name must be a non-empty string",
      'registerWorkflow: "bad" firstStep "missing" is not a declared step',
      "registerAgent: agent.name must be a non-empty string",
    ]);
  });

  test("rejects a reserved single:* name as a diagnostic and never adds it to the map", () => {
    const r = new ExtensionRegistry();
    r.addAgent("s", agent("dev"));
    // An extension trying to claim the synthetic namespace is rejected ...
    r.addWorkflow("sample-ext", { name: "single:dev", firstStep: "do", steps: { do: { agent: "dev" } } });

    expect(r.workflowNames()).toEqual([]);
    expect(r.listWorkflows()).toEqual([]); // never leaks into the list ...
    expect(r.diagnostics).toEqual([
      {
        source: "sample-ext",
        error: 'registerWorkflow: "single:dev" uses the reserved "single:*" synthetic-workflow namespace',
      },
    ]);
    // ... yet the builtin still resolves it from the live agent set (no drift).
    expect(r.resolveWorkflow("single:dev")?.steps).toEqual({ do: { agent: "dev" } });
  });

  test("diagnostics getter returns a defensive copy", () => {
    const r = new ExtensionRegistry();
    r.recordDiagnostic("a", "boom");
    const snap = r.diagnostics;
    snap.push({ source: "x", error: "y" });
    expect(r.diagnostics).toHaveLength(1);
  });
});

describe("singleStep builtin", () => {
  test("parseSingleStepName extracts the agent, or null", () => {
    expect(parseSingleStepName("single:dev")).toBe("dev");
    expect(parseSingleStepName("single:")).toBeNull();
    expect(parseSingleStepName("feature-dev")).toBeNull();
  });

  test("singleStepFor materialises a runnable one-step workflow for a known agent", () => {
    const r = new ExtensionRegistry();
    r.addAgent("s", agent("dev"));
    const built = r.singleStepFor("dev");
    expect(built).toEqual({
      name: "single:dev",
      firstStep: "do",
      steps: { do: { agent: "dev" } },
    });
  });

  test("singleStepFor throws for an unknown agent", () => {
    const r = new ExtensionRegistry();
    expect(() => r.singleStepFor("ghost")).toThrow(/unknown agent/);
  });

  test("resolveWorkflow materialises single:<agent> on demand but list/get hide it", () => {
    const r = new ExtensionRegistry();
    r.addAgent("s", agent("dev"));
    r.addWorkflow("s", wf("feature-dev"));

    // Resolves for the engine ...
    expect(r.resolveWorkflow("single:dev")?.steps).toEqual({ do: { agent: "dev" } });
    // ... and getWorkflowInfo renders it (so a client can inspect an enrolled task) ...
    expect(r.getWorkflowInfo("single:dev")?.name).toBe("single:dev");
    // ... but it NEVER leaks into the normal workflow list.
    expect(r.listWorkflows().map((w) => w.name)).toEqual(["feature-dev"]);
  });

  test("resolveWorkflow returns undefined for single:<agent> when the agent is gone", () => {
    const r = new ExtensionRegistry();
    expect(r.resolveWorkflow("single:dev")).toBeUndefined();
  });
});

describe("validatePosition (live-code hazard)", () => {
  test("ok for a known workflow + step", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("s", wf("feature-dev"));
    expect(r.validatePosition("feature-dev", "dev")).toEqual({ ok: true });
  });

  test("workflow_missing for an unknown workflow", () => {
    const r = new ExtensionRegistry();
    expect(r.validatePosition("ghost", "dev")).toEqual({
      ok: false,
      error: "workflow_missing: ghost",
    });
  });

  test("workflow_missing for a known workflow with an unknown step", () => {
    const r = new ExtensionRegistry();
    r.addWorkflow("s", wf("feature-dev"));
    expect(r.validatePosition("feature-dev", "ship")).toEqual({
      ok: false,
      error: "workflow_missing: feature-dev has no step ship",
    });
  });

  test("ok for a single:<agent> position when the agent exists", () => {
    const r = new ExtensionRegistry();
    r.addAgent("s", agent("dev"));
    expect(r.validatePosition("single:dev", "do")).toEqual({ ok: true });
  });
});

describe("renderWorkflowInfo (proto-v2 projection)", () => {
  test("per-step targets = every step (self included, sorted) + the three terminal statuses", () => {
    const info = renderWorkflowInfo(wf("feature-dev", { description: "two-stepper" }));
    expect(info.name).toBe("feature-dev");
    expect(info.description).toBe("two-stepper");
    expect(info.first_step).toBe("dev");
    expect(info.isolation).toBe("none");

    // Steps are rendered in sorted name order.
    expect(info.steps.map((s) => s.name)).toEqual(["accept", "dev", "review"]);

    const dev = info.steps.find((s) => s.name === "dev")!;
    expect(dev.agent).toBe("a1");
    expect(dev.human).toBe(false);
    // The superset projection declares EVERY step, including the self-loop
    // ({step:"dev"}) — a structurally valid retry target.
    expect(dev.targets).toEqual([
      { step: "accept" },
      { step: "dev" },
      { step: "review" },
      { status: "done" },
      { status: "cancel" },
      { status: "human" },
    ]);

    const accept = info.steps.find((s) => s.name === "accept")!;
    expect(accept.agent).toBeNull();
    expect(accept.human).toBe(true);
  });

  test("isolation tag is surfaced; description is omitted when absent", () => {
    const info = renderWorkflowInfo(
      wf("iso", {
        isolation: {
          tag: "worktree",
          async acquire() {
            return { cwd: "/tmp" };
          },
          async release() {},
        },
      }),
    );
    expect(info.isolation).toBe("worktree");
    expect("description" in info).toBe(false);
  });

  test("listAgents renders sorted AgentInfo[]", () => {
    const r = new ExtensionRegistry();
    r.addAgent("s", agent("zeta"));
    r.addAgent("s", agent("alpha"));
    expect(r.listAgents()).toEqual([{ name: "alpha" }, { name: "zeta" }]);
  });
});
