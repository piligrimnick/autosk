/**
 * The daemon-bundled extension discovery route (P6 step-4 decision / acceptance
 * #4): a fresh project, with no per-project files, discovers the shipped
 * `@autosk/feature-dev` workflow (with its four inline pi-agent steps) from the
 * bundled `daemon/extensions/` dir — so it can enroll into `feature-dev` out of
 * the box. The bundled dir is the LOWEST priority, and sibling library packages
 * (`pi-agent`, `worktree`) that declare no extension entry are NOT loaded.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

import { ProjectManager, ProjectRegistry, initProject, loadProjectRegistry } from "../src/index.ts";
import { tempDir } from "./helpers.ts";

const BUNDLED_DIR = fileURLToPath(new URL("../../extensions", import.meta.url));
/** The four agent steps of feature-dev (the step key IS the agent name). */
const AGENT_STEPS = ["dev", "docs", "review", "validator"];

describe("bundled extension discovery — feature-dev", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  test("loadProjectRegistry discovers feature-dev + its roles from the bundled dir", async () => {
    const project = tempDir();
    const home = tempDir();
    cleanups.push(project.cleanup, home.cleanup);
    await initProject(project.path);

    const registry = await loadProjectRegistry(project.path, { home: home.path, bundledDir: BUNDLED_DIR });

    const wf = registry.getWorkflowInfo("feature-dev");
    expect(wf).toBeDefined();
    expect(wf!.first_step).toBe("dev");
    expect(wf!.isolation).toBe("worktree");
    expect(wf!.steps.map((s) => s.name).sort()).toEqual(["accept", "dev", "docs", "review", "validator"]);
    // `accept` is a statusStep("human"); the four roles are agent steps (status null).
    expect(wf!.steps.find((s) => s.name === "accept")!.status).toBe("human");
    for (const name of AGENT_STEPS) {
      expect(wf!.steps.find((s) => s.name === name)!.status).toBeNull();
    }

    // The bundled discovery picks ONLY feature-dev; the sibling library packages
    // (pi-agent, worktree) declare no extension entry, so they contribute no
    // diagnostics from this route.
    expect(registry.diagnostics).toEqual([]);
  });

  test("a fresh project opened with a bundledDir can enroll into feature-dev", async () => {
    const project = tempDir();
    const home = tempDir();
    cleanups.push(project.cleanup, home.cleanup);
    await initProject(project.path);

    const pm = new ProjectManager({
      registry: new ProjectRegistry(`${home.path}/.autosk/projects.json`),
      store: { watch: false },
      extensions: { home: home.path, bundledDir: BUNDLED_DIR },
    });
    try {
      const handle = await pm.resolve(project.path);
      // The workflow is present in the project's registry → `task.enroll
      // {workflow:"feature-dev"}` will resolve it (the engine path is exercised
      // separately in engine.featuredev.test.ts).
      expect(handle.extensions.resolveWorkflow("feature-dev")).toBeDefined();
    } finally {
      await pm.close();
    }
  });
});
