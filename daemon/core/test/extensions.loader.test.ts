/**
 * Extension loader (plan §3.6): fixture-based discovery across all three
 * sources (project-local dir, global dir, npm packages from settings.json) and
 * all three per-dir shapes (direct file, subdir/index, package.json manifest),
 * deterministic merge/priority order, error isolation, and the no-trust rule.
 *
 * Fixtures are written into throwaway temp dirs as plain default-export
 * factories (no `@autosk/sdk` import — the AutoskAPI is duck-typed — so the
 * /tmp module resolves with zero node_modules wiring).
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

import { loadProjectRegistry, resolveProjectEntries } from "../src/index.ts";
import { tempDir } from "./helpers.ts";

/** A default-export factory that registers one workflow with an inline agent step. */
function extFile(workflow: string, opts: { description?: string } = {}): string {
  const desc = opts.description ? `description: ${JSON.stringify(opts.description)}, ` : "";
  return [
    "export default function (autosk) {",
    `  autosk.registerWorkflow({ name: ${JSON.stringify(workflow)}, ${desc}firstStep: "do", steps: { do: { onRun: async () => {} } } });`,
    "}",
    "",
  ].join("\n");
}

function write(path: string, content: string): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, content);
}

describe("extension loader — discovery sources × shapes", () => {
  let dir: ReturnType<typeof tempDir>;
  let proj: string;
  let home: string;

  beforeEach(() => {
    dir = tempDir();
    proj = join(dir.path, "proj");
    home = join(dir.path, "home");
  });
  afterEach(() => dir.cleanup());

  test("loads every source and every per-dir shape into one registry", async () => {
    // (a) project-local dir — all three shapes.
    write(join(proj, ".autosk/extensions/a-direct.ts"), extFile("a-direct")); // direct file
    write(join(proj, ".autosk/extensions/b-index/index.ts"), extFile("b-index")); // subdir/index
    write(
      join(proj, ".autosk/extensions/c-pkg/package.json"),
      JSON.stringify({ name: "c-pkg", autosk: { extensions: ["./entry.ts"] } }),
    );
    write(join(proj, ".autosk/extensions/c-pkg/entry.ts"), extFile("c-pkg")); // package manifest

    // (b) global dir — direct file.
    write(join(home, ".autosk/extensions/g-direct.ts"), extFile("g-direct"));

    // (c) npm packages from settings.json — project + global settings.
    write(join(proj, ".autosk/settings.json"), JSON.stringify({ extensions: ["@scope/proj-pkg"] }));
    write(join(home, ".autosk/settings.json"), JSON.stringify({ extensions: ["@scope/global-pkg"] }));
    // proj-pkg: package.json#autosk.extensions shape.
    write(
      join(home, ".autosk/packages/node_modules/@scope/proj-pkg/package.json"),
      JSON.stringify({ name: "@scope/proj-pkg", autosk: { extensions: ["./index.ts"] } }),
    );
    write(join(home, ".autosk/packages/node_modules/@scope/proj-pkg/index.ts"), extFile("proj-pkg"));
    // global-pkg: package.json with NO autosk field → index.ts fallback shape.
    write(
      join(home, ".autosk/packages/node_modules/@scope/global-pkg/package.json"),
      JSON.stringify({ name: "@scope/global-pkg" }),
    );
    write(join(home, ".autosk/packages/node_modules/@scope/global-pkg/index.ts"), extFile("global-pkg"));

    const registry = await loadProjectRegistry(proj, { home });

    expect(registry.workflowNames()).toEqual([
      "a-direct",
      "b-index",
      "c-pkg",
      "g-direct",
      "global-pkg",
      "proj-pkg",
    ]);
    expect(registry.diagnostics).toEqual([]);
  });

  test("the resolved entry order is project dir → global dir → npm packages", () => {
    write(join(proj, ".autosk/extensions/p.ts"), extFile("p"));
    write(join(home, ".autosk/extensions/g.ts"), extFile("g"));
    write(join(proj, ".autosk/settings.json"), JSON.stringify({ extensions: ["@s/proj"] }));
    write(join(home, ".autosk/settings.json"), JSON.stringify({ extensions: ["@s/global"] }));
    write(
      join(home, ".autosk/packages/node_modules/@s/proj/package.json"),
      JSON.stringify({ autosk: { extensions: ["./i.ts"] } }),
    );
    write(join(home, ".autosk/packages/node_modules/@s/proj/i.ts"), extFile("proj"));
    write(
      join(home, ".autosk/packages/node_modules/@s/global/package.json"),
      JSON.stringify({ autosk: { extensions: ["./i.ts"] } }),
    );
    write(join(home, ".autosk/packages/node_modules/@s/global/i.ts"), extFile("global"));

    const { entries } = resolveProjectEntries(proj, { home });
    expect(entries.map((e) => e.source)).toEqual([
      join(proj, ".autosk/extensions/p.ts"), // 1: project dir
      join(home, ".autosk/extensions/g.ts"), // 2: global dir
      "@s/proj", // 3a: project-settings package (project beats global)
      "@s/global", // 3b: global-settings package
    ]);
  });
});

describe("extension loader — deterministic merge / priority", () => {
  let dir: ReturnType<typeof tempDir>;
  let proj: string;
  let home: string;

  beforeEach(() => {
    dir = tempDir();
    proj = join(dir.path, "proj");
    home = join(dir.path, "home");
  });
  afterEach(() => dir.cleanup());

  test("project beats global beats npm on a name collision; losers are diagnostics", async () => {
    write(join(proj, ".autosk/extensions/p.ts"), extFile("shared", { description: "from-project" }));
    write(join(home, ".autosk/extensions/g.ts"), extFile("shared", { description: "from-global" }));
    write(join(proj, ".autosk/settings.json"), JSON.stringify({ extensions: ["@s/pkg"] }));
    write(
      join(home, ".autosk/packages/node_modules/@s/pkg/package.json"),
      JSON.stringify({ autosk: { extensions: ["./i.ts"] } }),
    );
    write(join(home, ".autosk/packages/node_modules/@s/pkg/i.ts"), extFile("shared", { description: "from-pkg" }));

    const registry = await loadProjectRegistry(proj, { home });

    // The highest-priority source (project dir) keeps the name.
    expect(registry.getWorkflowInfo("shared")?.description).toBe("from-project");
    // The two losers are recorded, in priority order (global before npm).
    expect(registry.diagnostics).toEqual([
      { source: join(home, ".autosk/extensions/g.ts"), error: "duplicate workflow name: shared" },
      { source: "@s/pkg", error: "duplicate workflow name: shared" },
    ]);
  });

  test("within a directory, sorted filename order decides the winner", async () => {
    write(join(proj, ".autosk/extensions/01-first.ts"), extFile("dup", { description: "first" }));
    write(join(proj, ".autosk/extensions/02-second.ts"), extFile("dup", { description: "second" }));

    const registry = await loadProjectRegistry(proj, { home });
    expect(registry.getWorkflowInfo("dup")?.description).toBe("first");
    expect(registry.diagnostics).toEqual([
      { source: join(proj, ".autosk/extensions/02-second.ts"), error: "duplicate workflow name: dup" },
    ]);
  });
});

describe("extension loader — error isolation (daemon stays up)", () => {
  let dir: ReturnType<typeof tempDir>;
  let proj: string;
  let home: string;

  beforeEach(() => {
    dir = tempDir();
    proj = join(dir.path, "proj");
    home = join(dir.path, "home");
  });
  afterEach(() => dir.cleanup());

  test("a throwing factory and a duplicate name surface in diagnostics; the rest register", async () => {
    const ext = (p: string) => join(proj, ".autosk/extensions", p);
    // Sorted order: good-a, good-b, nodefault, throws, zdup.
    write(ext("good-a.ts"), extFile("alpha"));
    write(ext("good-b.ts"), extFile("beta"));
    write(ext("nodefault.ts"), "export const notAFactory = 42;\n");
    write(ext("throws.ts"), 'export default function () { throw new Error("kaboom"); }\n');
    // zdup registers the duplicate workflow "alpha".
    write(
      ext("zdup.ts"),
      [
        "export default function (autosk) {",
        '  autosk.registerWorkflow({ name: "alpha", firstStep: "do", steps: { do: { onRun: async () => {} } } });',
        "}",
        "",
      ].join("\n"),
    );

    // loadProjectRegistry never throws — the daemon stays up.
    const registry = await loadProjectRegistry(proj, { home });

    // The good extensions registered; "alpha" stayed the good-a definition.
    expect(registry.workflowNames()).toEqual(["alpha", "beta"]);
    // The registry is fully usable afterwards.
    expect(registry.listWorkflows().map((w) => w.name)).toEqual(["alpha", "beta"]);

    const bySource = Object.fromEntries(registry.diagnostics.map((d) => [d.source, d.error]));
    expect(bySource[ext("nodefault.ts")]).toBe("extension has no default-export factory function");
    expect(bySource[ext("throws.ts")]).toContain("factory threw: kaboom");
    expect(bySource[ext("zdup.ts")]).toBe("duplicate workflow name: alpha");
    expect(registry.diagnostics).toHaveLength(3);
  });

  test("an extension using the removed registerAgent or a string agent step surfaces a diagnostic", async () => {
    const ext = (p: string) => join(proj, ".autosk/extensions", p);
    // (a) Calls the removed registerAgent → the AutoskAPI handle has no such
    //     method, so the factory throws and is recorded (daemon stays up).
    write(
      ext("a-legacy-agent.ts"),
      [
        "export default function (autosk) {",
        '  autosk.registerAgent({ name: "x", async onRun() {} });',
        "}",
        "",
      ].join("\n"),
    );
    // (b) Registers a workflow with a v1 string-`agent` step → step-shape
    //     validation rejects it as a diagnostic.
    write(
      ext("b-string-step.ts"),
      [
        "export default function (autosk) {",
        '  autosk.registerWorkflow({ name: "legacy", firstStep: "do", steps: { do: { agent: "x" } } });',
        "}",
        "",
      ].join("\n"),
    );
    // A good extension alongside still loads.
    write(ext("c-good.ts"), extFile("good"));

    const registry = await loadProjectRegistry(proj, { home });

    expect(registry.workflowNames()).toEqual(["good"]);
    const bySource = Object.fromEntries(registry.diagnostics.map((d) => [d.source, d.error]));
    expect(bySource[ext("a-legacy-agent.ts")]).toMatch(/factory threw:/);
    expect(bySource[ext("b-string-step.ts")]).toBe(
      'registerWorkflow: "legacy" step "do" must be an agent (with onRun) or a statusStep',
    );
  });

  test("a malformed extension module is recorded as a failed import, not thrown", async () => {
    write(join(proj, ".autosk/extensions/broken.ts"), "export default function ( {  // syntax error\n");
    write(join(proj, ".autosk/extensions/ok.ts"), extFile("ok"));

    const registry = await loadProjectRegistry(proj, { home });
    expect(registry.workflowNames()).toEqual(["ok"]);
    const diag = registry.diagnostics.find((d) => d.source.endsWith("broken.ts"));
    expect(diag?.error).toMatch(/^failed to import:/);
  });

  test("a malformed package.json (null / non-object) contributes nothing; the project still opens", async () => {
    // A syntactically valid package.json whose top-level value is the JSON
    // literal `null` (and a sibling whose value is a non-object) must NOT crash
    // discovery (`pkg.autosk` on a null would throw). The subdirs contribute
    // nothing, no diagnostic, and a sibling good extension still registers.
    write(join(proj, ".autosk/extensions/weird-null/package.json"), "null");
    write(join(proj, ".autosk/extensions/weird-num/package.json"), "42");
    write(join(proj, ".autosk/extensions/weird-arr/package.json"), '["./x.ts"]');
    write(join(proj, ".autosk/extensions/good.ts"), extFile("good"));

    // loadProjectRegistry must NOT throw — the daemon stays up.
    const registry = await loadProjectRegistry(proj, { home });

    expect(registry.workflowNames()).toEqual(["good"]);
    expect(registry.diagnostics).toEqual([]);
  });
});

describe("extension loader — settings package diagnostics", () => {
  let dir: ReturnType<typeof tempDir>;
  let proj: string;
  let home: string;

  beforeEach(() => {
    dir = tempDir();
    proj = join(dir.path, "proj");
    home = join(dir.path, "home");
  });
  afterEach(() => dir.cleanup());

  test("a settings-listed package that is not installed surfaces a diagnostic (not silence)", async () => {
    // A real local extension loads fine alongside the broken package reference.
    write(join(proj, ".autosk/extensions/local.ts"), extFile("local"));
    write(join(proj, ".autosk/settings.json"), JSON.stringify({ extensions: ["@scope/missing"] }));

    const registry = await loadProjectRegistry(proj, { home });

    expect(registry.workflowNames()).toEqual(["local"]);
    // The operator listed @scope/missing but never installed it: recorded
    // against the package NAME so `project.diagnostics` reflects their intent.
    const diag = registry.diagnostics.find((d) => d.source === "@scope/missing");
    expect(diag?.error).toMatch(/not installed/);
  });

  test("a settings-listed package present but declaring no extension is a distinct diagnostic", async () => {
    write(join(proj, ".autosk/settings.json"), JSON.stringify({ extensions: ["@scope/empty"] }));
    // Installed (package.json present) but no autosk.extensions and no index.ts.
    write(
      join(home, ".autosk/packages/node_modules/@scope/empty/package.json"),
      JSON.stringify({ name: "@scope/empty" }),
    );

    const registry = await loadProjectRegistry(proj, { home });
    const diag = registry.diagnostics.find((d) => d.source === "@scope/empty");
    expect(diag?.error).toMatch(/declares no extension/);
  });
});

describe("extension loader — reload semantics (daemon-start is the truth)", () => {
  let dir: ReturnType<typeof tempDir>;
  let proj: string;
  let home: string;

  beforeEach(() => {
    dir = tempDir();
    proj = join(dir.path, "proj");
    home = join(dir.path, "home");
  });
  afterEach(() => dir.cleanup());

  // Plan §3.6: "the registry at daemon start is the truth." A registry is built
  // once per process and the module graph is cached by specifier (standard ESM
  // semantics — a query-string cache-bust is ignored for file:// URLs in Bun,
  // and there is no reliable in-process ESM cache invalidation). So editing an
  // extension is reflected only after a DAEMON RESTART, not on an in-process
  // re-load. This test pins that contract so nobody mistakes it for hot-reload.
  test("an in-process re-load returns the ORIGINAL definition (edits need a restart)", async () => {
    const entry = join(proj, ".autosk/extensions/wf.ts");
    write(entry, extFile("flow", { description: "v1" }));

    const first = await loadProjectRegistry(proj, { home });
    expect(first.getWorkflowInfo("flow")?.description).toBe("v1");

    // Rewrite the SAME entry file and re-load within the same process: the
    // cached module is reused, so the v1 definition is what we still see.
    write(entry, extFile("flow", { description: "v2" }));
    const second = await loadProjectRegistry(proj, { home });

    expect(second.getWorkflowInfo("flow")?.description).toBe("v1");
  });

  // The flip side: a never-before-imported entry path is always read fresh from
  // disk at open time (a fresh daemon hits this for every extension), so the
  // registry always reflects the on-disk code as of the process that built it.
  test("a not-yet-imported entry is read fresh from disk at open time", async () => {
    write(join(proj, ".autosk/extensions/fresh-at-open.ts"), extFile("fresh-at-open", { description: "on-disk" }));
    const reg = await loadProjectRegistry(proj, { home });
    expect(reg.getWorkflowInfo("fresh-at-open")?.description).toBe("on-disk");
  });
});

describe("extension loader — no trust model", () => {
  let dir: ReturnType<typeof tempDir>;

  beforeEach(() => dir = tempDir());
  afterEach(() => dir.cleanup());

  test("a never-before-seen extension is loaded immediately, with no approval gate", async () => {
    const proj = join(dir.path, "proj");
    const home = join(dir.path, "home");
    write(join(proj, ".autosk/extensions/fresh.ts"), extFile("fresh"));

    // No prompt/approval callback exists in the signature; placing the file IS
    // the consent. If the loader gated on a prompt, this await would hang.
    const registry = await loadProjectRegistry(proj, { home });
    expect(registry.workflowNames()).toEqual(["fresh"]);
    expect(registry.diagnostics).toEqual([]);
  });
});
