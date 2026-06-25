/**
 * `ProjectManager.rebuildRegistry` (extension hot-reload, plan §1-2): rebuilds a
 * project's registry from disk and atomically swaps it onto the cached handle,
 * parking invalid `work` tasks — but never out from under a live session.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

import { newSessionId } from "@autosk/sdk";

import {
  canonicalize,
  CapturingLogger,
  initProject,
  ProjectManager,
  ProjectRegistry,
} from "../src/index.ts";
import { tempDir } from "./helpers.ts";

/** A local extension file registering a one-step `go → done` workflow named `name`. */
function wfExt(name: string): string {
  return [
    "export default function (autosk) {",
    `  autosk.registerWorkflow({ name: ${JSON.stringify(name)}, firstStep: "go", steps: {`,
    "    go: { onRun: async (ctx) => { await ctx.transit({ status: \"done\" }); } },",
    "  } });",
    "}",
    "",
  ].join("\n");
}

function write(path: string, content: string): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, content);
}

describe("ProjectManager.rebuildRegistry", () => {
  let dir: ReturnType<typeof tempDir>;
  let mgr: ProjectManager;
  let proj: string;
  let home: string;
  let logger: CapturingLogger;
  const extFile = (root: string) => join(root, ".autosk/extensions/wf.ts");

  beforeEach(async () => {
    dir = tempDir();
    proj = join(dir.path, "proj");
    home = join(dir.path, "home");
    await initProject(proj);
    logger = new CapturingLogger();
    mgr = new ProjectManager({
      registry: new ProjectRegistry(join(home, ".autosk", "projects.json")),
      store: { watch: false },
      extensions: { home },
      logger,
    });
  });
  afterEach(async () => {
    await mgr.close();
    dir.cleanup();
  });

  test("picks up a newly-added extension and swaps it onto the cached handle (no re-open)", async () => {
    const handle = await mgr.open(await canonicalize(proj));
    expect(handle.extensions.workflowNames()).toEqual([]);

    // Add a new local extension AFTER the project was opened.
    write(extFile(proj), wfExt("added"));
    const summary = await mgr.rebuildRegistry(handle.root);

    expect(summary.open).toBe(true);
    expect(summary.workflows).toEqual(["added"]);
    expect(summary.parked).toEqual([]);
    // The SAME cached handle now exposes the rebuilt registry (swapped in place).
    expect(handle.extensions.workflowNames()).toEqual(["added"]);
  });

  test("drops a removed workflow and parks a non-live work task on it", async () => {
    write(extFile(proj), wfExt("rm"));
    const handle = await mgr.open(await canonicalize(proj));
    expect(handle.extensions.workflowNames()).toEqual(["rm"]);

    // A non-live `work` task sitting on the workflow (no session).
    const t = await handle.store.createTask({ title: "on rm" });
    await handle.store.setPosition(t.id, { status: "work", workflow: "rm", step: "go" });

    rmSync(extFile(proj));
    const summary = await mgr.rebuildRegistry(handle.root);

    expect(summary.workflows).toEqual([]);
    expect(summary.parked.map((p) => p.taskId)).toEqual([t.id]);
    expect(handle.extensions.workflowNames()).toEqual([]);
    const view = await handle.store.taskView(t.id);
    expect(view.status).toBe("human");
    expect((await handle.store.listComments(t.id)).at(-1)).toMatchObject({
      author: "autosk",
      text: "workflow_missing: rm",
    });
  });

  test("does NOT park a task that has a live session (skip-live)", async () => {
    write(extFile(proj), wfExt("rm"));
    const handle = await mgr.open(await canonicalize(proj));

    const t = await handle.store.createTask({ title: "running on rm" });
    await handle.store.setPosition(t.id, { status: "work", workflow: "rm", step: "go" });
    // Simulate a live (queued) session captured the now-removed workflow.
    await handle.store.sessions.create({
      id: newSessionId(),
      task_id: t.id,
      workflow: "rm",
      step: "go",
      agent: "go",
      cwd: handle.root,
      timestamp: new Date().toISOString(),
    });
    expect(handle.store.sessions.hasLiveSession(t.id)).toBe(true);

    rmSync(extFile(proj));
    const summary = await mgr.rebuildRegistry(handle.root);

    // The workflow is gone from the registry, but the live task is NOT parked —
    // it self-heals via the engine's park-on-missing dispatch path once it ends.
    expect(summary.workflows).toEqual([]);
    expect(summary.parked).toEqual([]);
    expect((await handle.store.taskView(t.id)).status).toBe("work");
  });

  test("a bad extension on reload is recorded as a diagnostic; the rest of the registry stays usable (never throws)", async () => {
    // Open with a GOOD workflow already loaded.
    write(join(proj, ".autosk/extensions/good.ts"), wfExt("good"));
    const handle = await mgr.open(await canonicalize(proj));
    expect(handle.extensions.workflowNames()).toEqual(["good"]);

    // Drop a BAD extension whose factory throws, alongside a SECOND good one. The
    // rebuild must isolate the failure: record it as a diagnostic, keep both good
    // workflows, and resolve (never throw).
    write(join(proj, ".autosk/extensions/bad.ts"), "export default function () { throw new Error('boom'); }\n");
    write(join(proj, ".autosk/extensions/good2.ts"), wfExt("good2"));

    const summary = await mgr.rebuildRegistry(handle.root);

    // The bad extension surfaced as a diagnostic, not a crash.
    expect(summary.diagnostics.length).toBeGreaterThan(0);
    expect(summary.diagnostics.some((d) => /bad\.ts$/.test(d.source) && /factory threw/.test(d.error))).toBe(true);
    // The rest of the registry stays usable: both good workflows loaded + swapped in.
    expect(summary.workflows).toEqual(["good", "good2"]);
    expect(handle.extensions.workflowNames()).toEqual(["good", "good2"]);
    expect(handle.extensions.diagnostics.length).toBeGreaterThan(0);
  });

  test("a project that is not open is a no-op marker (open:false)", async () => {
    const summary = await mgr.rebuildRegistry(await canonicalize(proj));
    expect(summary.open).toBe(false);
    expect(summary.registry).toBeUndefined();
    expect(summary.workflows).toEqual([]);
    expect(summary.parked).toEqual([]);
  });
});
