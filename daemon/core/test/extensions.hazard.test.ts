/**
 * Live-code hazard guard (plan §3.6, step 5) + the project-manager integration
 * that runs it on open.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

import {
  canonicalize,
  CapturingLogger,
  ExtensionRegistry,
  initProject,
  ProjectManager,
  ProjectRegistry,
  Store,
  validateInFlightTasks,
} from "../src/index.ts";
import type { AgentDefinition, WorkflowDefinition } from "@autosk/sdk";
import { fixedClock, tempDir } from "./helpers.ts";

const agentStep = (): AgentDefinition => ({ async onRun() {} });
const featureDev = (): WorkflowDefinition => ({
  name: "feature-dev",
  firstStep: "dev",
  steps: { dev: agentStep(), review: agentStep() },
});

function write(path: string, content: string): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, content);
}

describe("validateInFlightTasks", () => {
  let dir: ReturnType<typeof tempDir>;
  let store: Store;

  beforeEach(async () => {
    dir = tempDir();
    store = new Store(dir.path, {
      watch: false,
      clock: fixedClock(["t0", "t1", "t2", "t3", "t4", "t5"]),
    });
    await store.open();
  });
  afterEach(async () => {
    await store.close();
    dir.cleanup();
  });

  test("parks a work task whose workflow is absent; leaves a workflow_missing comment", async () => {
    const t = await store.createTask({ title: "orphaned" });
    await store.setPosition(t.id, { status: "work", workflow: "feature-dev", step: "dev" });

    const registry = new ExtensionRegistry(); // empty — feature-dev is gone
    const parked = await validateInFlightTasks(store, registry);

    expect(parked).toEqual([
      { taskId: t.id, workflow: "feature-dev", step: "dev", error: "workflow_missing: feature-dev" },
    ]);

    const view = await store.taskView(t.id);
    expect(view.status).toBe("human");
    // The position is preserved so the operator can see what it pointed at.
    expect(view.workflow).toBe("feature-dev");
    expect(view.step).toBe("dev");

    const comments = await store.listComments(t.id);
    expect(comments).toHaveLength(1);
    expect(comments[0]).toMatchObject({ author: "autosk", text: "workflow_missing: feature-dev" });
  });

  test("parks a work task whose step is absent from a known workflow", async () => {
    const t = await store.createTask({ title: "bad step" });
    await store.setPosition(t.id, { status: "work", workflow: "feature-dev", step: "ship" });

    const registry = new ExtensionRegistry();
    registry.addWorkflow("s", featureDev());
    const parked = await validateInFlightTasks(store, registry);

    expect(parked).toHaveLength(1);
    expect(parked[0]!.error).toBe("workflow_missing: feature-dev has no step ship");
    expect((await store.taskView(t.id)).status).toBe("human");
  });

  test("a valid in-flight task is left completely untouched", async () => {
    const t = await store.createTask({ title: "healthy" });
    await store.setPosition(t.id, { status: "work", workflow: "feature-dev", step: "dev" });

    const registry = new ExtensionRegistry();
    registry.addWorkflow("s", featureDev());

    const before = await store.taskView(t.id);
    const parked = await validateInFlightTasks(store, registry);
    const after = await store.taskView(t.id);

    expect(parked).toEqual([]);
    expect(after).toEqual(before); // same status, updated_at, comment_count, …
    expect(await store.listComments(t.id)).toEqual([]);
  });

  test("new / done tasks are ignored; an already-human task is not re-parked (no spam)", async () => {
    const fresh = await store.createTask({ title: "new" }); // status=new
    const finished = await store.createTask({ title: "done" });
    await store.setPosition(finished.id, { status: "done", workflow: "feature-dev", step: "dev" });
    const parkedAlready = await store.createTask({ title: "already parked" });
    await store.setPosition(parkedAlready.id, { status: "human", workflow: "feature-dev", step: "dev" });

    const registry = new ExtensionRegistry(); // feature-dev absent
    const parked = await validateInFlightTasks(store, registry);

    // Nothing is parked: new/done are out of scope, and the human task is
    // already safely off the scheduler — re-commenting would be noise.
    expect(parked).toEqual([]);
    expect((await store.taskView(fresh.id)).status).toBe("new");
    expect((await store.taskView(finished.id)).status).toBe("done");
    expect((await store.taskView(parkedAlready.id)).status).toBe("human");
    expect(await store.listComments(parkedAlready.id)).toEqual([]);
  });

  test("parks a work task with a null workflow/step (inconsistent enrolment)", async () => {
    const t = await store.createTask({ title: "half-enrolled" });
    // status=work but no workflow/step — the bad state an external hand-edit can
    // leave behind (flip status to work without enrolling). The scheduler can
    // never pick it up, so the guard parks it rather than letting it stall.
    await store.setPosition(t.id, { status: "work", workflow: null, step: null });

    const registry = new ExtensionRegistry();
    const parked = await validateInFlightTasks(store, registry);

    expect(parked).toEqual([
      {
        taskId: t.id,
        workflow: null,
        step: null,
        error: "workflow_missing: enrolled task has no workflow/step",
      },
    ]);
    expect((await store.taskView(t.id)).status).toBe("human");
    const comments = await store.listComments(t.id);
    expect(comments[0]).toMatchObject({
      author: "autosk",
      text: "workflow_missing: enrolled task has no workflow/step",
    });
  });

  test("a human task with a null workflow/step is left untouched (already off the scheduler)", async () => {
    const t = await store.createTask({ title: "parked half-enrolled" });
    await store.setPosition(t.id, { status: "human", workflow: null, step: null });

    const registry = new ExtensionRegistry();
    const parked = await validateInFlightTasks(store, registry);

    expect(parked).toEqual([]);
    expect((await store.taskView(t.id)).status).toBe("human");
    expect(await store.listComments(t.id)).toEqual([]);
  });

});

describe("ProjectManager open() runs the loader + hazard guard", () => {
  let dir: ReturnType<typeof tempDir>;
  let mgr: ProjectManager;
  let proj: string;
  let home: string;
  let logger: CapturingLogger;

  beforeEach(async () => {
    dir = tempDir();
    proj = join(dir.path, "proj");
    home = join(dir.path, "home");
    await initProject(proj);
    // Inject a capturing logger: it quiets the suite (no warn lines printed to
    // test output) AND lets us assert the open-time log breadcrumbs.
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

  test("loads project extensions into the handle and parks orphaned in-flight tasks", async () => {
    // The project ships a feature-dev workflow as a local extension.
    write(
      join(proj, ".autosk/extensions/wf.ts"),
      [
        "export default function (autosk) {",
        '  autosk.registerWorkflow({ name: "feature-dev", firstStep: "dev", steps: { dev: { onRun: async () => {} }, review: { onRun: async () => {} } } });',
        "}",
        "",
      ].join("\n"),
    );

    // Pre-seed two in-flight tasks on disk: one valid, one pointing at a
    // workflow that no extension defines.
    const seed = new Store(proj, { watch: false });
    await seed.open();
    const healthy = await seed.createTask({ title: "healthy" });
    await seed.setPosition(healthy.id, { status: "work", workflow: "feature-dev", step: "dev" });
    const orphan = await seed.createTask({ title: "orphan" });
    await seed.setPosition(orphan.id, { status: "work", workflow: "ghost-flow", step: "go" });
    await seed.close();

    const handle = await mgr.open(await canonicalize(proj));

    // The registry is exposed on the handle.
    expect(handle.extensions.workflowNames()).toEqual(["feature-dev"]);
    expect(handle.extensions.diagnostics).toEqual([]);

    // The orphan was parked to human with a workflow_missing comment ...
    const orphanView = await handle.store.taskView(orphan.id);
    expect(orphanView.status).toBe("human");
    const comments = await handle.store.listComments(orphan.id);
    expect(comments[0]).toMatchObject({ author: "autosk", text: "workflow_missing: ghost-flow" });

    // ... while the healthy task is left running.
    expect((await handle.store.taskView(healthy.id)).status).toBe("work");
  });

  test("open() logs a load-diagnostics summary and one line per hazard park", async () => {
    // One broken extension (a throwing factory) → a load diagnostic; plus one
    // orphaned work task → a hazard park. Both must leave a daemon-log
    // breadcrumb (not only the structured project.diagnostics / on-disk park).
    write(join(proj, ".autosk/extensions/throws.ts"), 'export default function () { throw new Error("kaboom"); }\n');

    const seed = new Store(proj, { watch: false });
    await seed.open();
    const orphan = await seed.createTask({ title: "orphan" });
    await seed.setPosition(orphan.id, { status: "work", workflow: "ghost-flow", step: "go" });
    await seed.close();

    await mgr.open(await canonicalize(proj));

    // Exactly one load-diagnostics summary warn, naming the offending source.
    const diagWarns = logger.warns.filter((w) => w.includes("load diagnostic"));
    expect(diagWarns).toHaveLength(1);
    expect(diagWarns[0]).toContain("throws.ts");
    expect(diagWarns[0]).toContain("project.diagnostics");

    // Exactly one hazard-park warn for the orphaned task.
    const parkWarns = logger.warns.filter((w) => w.includes("live-code hazard: parked"));
    expect(parkWarns).toHaveLength(1);
    expect(parkWarns[0]).toContain(orphan.id);
    expect(parkWarns[0]).toContain("workflow_missing: ghost-flow");
  });

});
