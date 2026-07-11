/**
 * Extension hot-reload over the live daemon (plan §4): `ext add` hot-applies to
 * open projects (scoped: a global add reloads every open project, a `-l` add
 * reloads only that one), `extension.reload` makes a new workflow immediately
 * schedulable with no restart, and a reload emits `registry-changed`.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

import type {
  ExtensionInstallResult,
  ExtensionReloadResult,
  TaskView,
  WorkflowInfo,
} from "@autosk/sdk";

import { startTestDaemon, type RpcClient, type TestDaemon } from "./rpcHarness.ts";
import { waitFor } from "./helpers.ts";

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

const names = (ws: WorkflowInfo[]): string[] => ws.map((w) => w.name);

describe("extension hot-reload (live daemon)", () => {
  let td: TestDaemon;
  let c: RpcClient;
  beforeEach(async () => {
    td = await startTestDaemon();
    c = await td.client();
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("a global ext add hot-reloads every open project", async () => {
    const p1 = await td.makeProject("p1");
    const p2 = await td.makeProject("p2");
    // Open both projects (resolveHandle) so they are loaded + engine-registered.
    await c.call("registry.workflow.list", { cwd: p1 });
    await c.call("registry.workflow.list", { cwd: p2 });

    const extFile = join(td.dir, "global-ext", "wf.ts");
    write(extFile, wfExt("global-wf"));

    const res = await c.call<ExtensionInstallResult>("extension.install", { cwd: p1, source: extFile });
    expect(res.scope).toBe("global");
    expect(res.reloaded).toBe(true);
    expect(res.reloaded_projects).toBe(2);

    // Both projects see the new workflow without a restart.
    const w1 = await c.call<WorkflowInfo[]>("registry.workflow.list", { cwd: p1 });
    const w2 = await c.call<WorkflowInfo[]>("registry.workflow.list", { cwd: p2 });
    expect(names(w1)).toContain("global-wf");
    expect(names(w2)).toContain("global-wf");
  });

  test("a project-scoped (-l) ext add hot-reloads only that project", async () => {
    const p1 = await td.makeProject("p1");
    const p2 = await td.makeProject("p2");
    await c.call("registry.workflow.list", { cwd: p1 });
    await c.call("registry.workflow.list", { cwd: p2 });

    const extFile = join(td.dir, "proj-ext", "wf.ts");
    write(extFile, wfExt("proj-wf"));

    const res = await c.call<ExtensionInstallResult>("extension.install", { cwd: p1, source: extFile, local: true });
    expect(res.scope).toBe("project");
    expect(res.reloaded).toBe(true);
    expect(res.reloaded_projects).toBe(1);

    const w1 = await c.call<WorkflowInfo[]>("registry.workflow.list", { cwd: p1 });
    const w2 = await c.call<WorkflowInfo[]>("registry.workflow.list", { cwd: p2 });
    expect(names(w1)).toContain("proj-wf");
    expect(names(w2)).not.toContain("proj-wf");
  });

  test("extension.reload makes a new workflow immediately schedulable (enroll dispatches, no restart)", async () => {
    const proj = await td.makeProject("proj");
    // Open it first; the workflow does not exist yet.
    const before = await c.call<WorkflowInfo[]>("registry.workflow.list", { cwd: proj });
    expect(names(before)).not.toContain("hot-wf");

    // Add a project-local extension on disk, then reload.
    write(join(proj, ".autosk/extensions/hot.ts"), wfExt("hot-wf"));
    const reload = await c.call<ExtensionReloadResult>("extension.reload", { cwd: proj });
    expect(reload.workflows).toContain("hot-wf");
    expect(reload.parked).toEqual([]);

    // A `new` task enrolled into the just-added workflow now dispatches + runs.
    const task = await c.call<TaskView>("task.create", { cwd: proj, title: "hot" });
    await c.call("task.enroll", { cwd: proj, id: task.id, workflow: "hot-wf" });
    await waitFor(async () => (await c.call<TaskView>("task.get", { cwd: proj, id: task.id })).status === "done");
  });

  test("a bad extension on reload is a diagnostic, not a crash; good workflows stay schedulable", async () => {
    const proj = await td.makeProject("proj");
    await c.call("registry.workflow.list", { cwd: proj }); // open + engine-register

    // A GOOD workflow next to a BAD extension whose factory throws on load.
    write(join(proj, ".autosk/extensions/good.ts"), wfExt("good-wf"));
    write(join(proj, ".autosk/extensions/bad.ts"), "export default function () { throw new Error('boom'); }\n");

    // The reload does NOT reject: it returns a result carrying the diagnostic.
    const reload = await c.call<ExtensionReloadResult>("extension.reload", { cwd: proj });
    expect(reload.diagnostics.length).toBeGreaterThan(0);
    expect(reload.diagnostics.some((d) => /factory threw/.test(d.error))).toBe(true);
    // The sibling good workflow loaded despite the bad neighbour.
    expect(reload.workflows).toContain("good-wf");

    // The same diagnostic is surfaced via project.diagnostics (fresh handle read).
    const diag = await c.call<{ extensions: { source: string; error: string }[] }>("project.diagnostics", { cwd: proj });
    expect(diag.extensions.some((d) => /factory threw/.test(d.error))).toBe(true);

    // And the good workflow is genuinely schedulable: enroll dispatches + runs to done.
    const task = await c.call<TaskView>("task.create", { cwd: proj, title: "good" });
    await c.call("task.enroll", { cwd: proj, id: task.id, workflow: "good-wf" });
    await waitFor(async () => (await c.call<TaskView>("task.get", { cwd: proj, id: task.id })).status === "done");
  });

  test("a hot-reload emits registry-changed to a project subscriber", async () => {
    const proj = await td.makeProject("proj");
    // task.subscribe opens the project AND records its root for root-scoped pushes.
    await c.call("task.subscribe", { cwd: proj });

    write(join(proj, ".autosk/extensions/notif.ts"), wfExt("notif-wf"));
    const note = c.waitForNotification((n) => n.method === "registry-changed");
    await c.call("extension.reload", { cwd: proj });
    const n = await note;
    expect(typeof (n.params as { root?: unknown }).root).toBe("string");
  });
});
