/**
 * `@autosk/gh-review` tests:
 *
 *  - `parseGhTarget`: URL extraction (issues/pull, embedded in prose, `www.`,
 *    dots/hyphens in owner/repo, query strings) and rejection (no URL, other
 *    hosts, http://, malformed paths);
 *  - token provisioning (`readGhToken` / `checkGhToken`): the operator's
 *    `ro-token.json` (`{ "token": "…" }`) — absent file, invalid JSON,
 *    missing/empty `token`, and the happy path;
 *  - `ghReviewGuard` (the `onTransit` enroll gate): rejects when the token or
 *    a parsable URL is missing (commenting the reason), allows everything else
 *    — non-`review` targets, re-entries (visits > 0), the description
 *    fallback — and comments the parsed target on a clean enroll;
 *  - `ghReviewSandbox`: the `docker run` argv carries `GH_TOKEN` (+ the
 *    no-write env) read FRESH from the token file, the pi config mount, the
 *    layered project `.git` bind mount just before the image token — and NO gh
 *    config mount;
 *  - `ghReviewWorkflow`: the shape (firstStep `review`, the three steps,
 *    `accept` a human park).
 */

import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { StatusStep, TaskView, TransitContext } from "@autosk/sdk";
import type { Sandbox } from "@autosk/sandbox";

import {
  checkGhToken,
  ghReviewGuard,
  ghReviewSandbox,
  ghReviewWorkflow,
  ghTokenFile,
  readGhToken,
} from "../src/index.ts";
import { parseGhTarget } from "../src/parse.ts";

// ---------------------------------------------------------------------------
// temp dirs + env save/restore.
// ---------------------------------------------------------------------------

const temps: string[] = [];
function mkTemp(prefix: string): string {
  const d = realpathSync(mkdtempSync(join(tmpdir(), prefix)));
  temps.push(d);
  return d;
}

const savedEnv = new Map<string, string | undefined>();
function setEnv(key: string, value: string | undefined): void {
  if (!savedEnv.has(key)) savedEnv.set(key, process.env[key]);
  if (value === undefined) delete process.env[key];
  else process.env[key] = value;
}

afterEach(() => {
  for (const [key, value] of savedEnv) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  savedEnv.clear();
  for (const d of temps.splice(0)) rmSync(d, { recursive: true, force: true });
});

/**
 * Points the gh token dir + pi config at temp dirs; optionally provisions
 * `ro-token.json` (as `{ token }` JSON, or verbatim `raw`).
 */
function fakeGhDir(opts: { token?: string; raw?: string } = {}): { ghDir: string; token: string | undefined } {
  const ghDir = mkTemp("ghrev-gh-");
  const token = opts.token;
  if (token !== undefined) writeFileSync(join(ghDir, "ro-token.json"), JSON.stringify({ token }));
  else if (opts.raw !== undefined) writeFileSync(join(ghDir, "ro-token.json"), opts.raw);
  setEnv("AUTOSK_GH_DIR", ghDir);
  // A real (empty) pi dir keeps buildMounts quiet in the sandbox tests.
  setEnv("AUTOSK_PI_DIR", mkTemp("ghrev-pi-"));
  return { ghDir, token };
}

// ---------------------------------------------------------------------------
// a fake TransitContext for the guard.
// ---------------------------------------------------------------------------

function fakeTask(title: string, description = ""): TaskView {
  return {
    id: "ask-test1",
    title,
    description,
    status: "new",
    workflow: null,
    step: null,
    blocked: false,
    blocked_by: [],
    blocks: [],
    comment_count: 0,
    metadata: {},
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

function fakeTransit(opts: { title: string; description?: string; visits?: number }): TransitContext & { comments: string[] } {
  const comments: string[] = [];
  const visitsCount = opts.visits ?? 0;
  const task = fakeTask(opts.title, opts.description);
  return {
    comments,
    taskId: task.id,
    workflow: "gh-review",
    step: "",
    visits: (_step: string) => visitsCount,
    tasks: {
      currentId: task.id,
      current: async () => task,
      get: async (_id: string) => task,
      list: async () => [task],
      comments: async () => [],
    },
    comment: async (text: string) => {
      comments.push(text);
    },
  };
}

// ---------------------------------------------------------------------------
// parseGhTarget
// ---------------------------------------------------------------------------

describe("parseGhTarget", () => {
  test("parses an issue URL", () => {
    expect(parseGhTarget("https://github.com/wierdbytes/autosk/issues/9")).toEqual({
      kind: "issue",
      owner: "wierdbytes",
      repo: "autosk",
      number: 9,
      url: "https://github.com/wierdbytes/autosk/issues/9",
    });
  });

  test("parses a pull URL", () => {
    expect(parseGhTarget("https://github.com/wierdbytes/autosk/pull/12")).toMatchObject({
      kind: "pull",
      owner: "wierdbytes",
      repo: "autosk",
      number: 12,
      url: "https://github.com/wierdbytes/autosk/pull/12",
    });
  });

  test("finds a URL embedded in prose (trailing punctuation ignored)", () => {
    const t = parseGhTarget("Please review https://github.com/foo/bar/pull/42/files, thanks.");
    expect(t).toMatchObject({ kind: "pull", owner: "foo", repo: "bar", number: 42 });
  });

  test("tolerates a leading www. and path casing", () => {
    const t = parseGhTarget("https://www.github.com/Foo/Bar/ISSUES/7");
    expect(t).toMatchObject({ kind: "issue", owner: "Foo", repo: "Bar", number: 7 });
    expect(t?.url).toBe("https://github.com/Foo/Bar/issues/7");
  });

  test("handles dots/hyphens in owner/repo and a query string", () => {
    expect(parseGhTarget("https://github.com/foo-bar/baz.qux/issues/3?q=1")).toMatchObject({
      kind: "issue",
      owner: "foo-bar",
      repo: "baz.qux",
      number: 3,
    });
  });

  test("rejects text with no URL / other hosts / plain http / malformed paths", () => {
    expect(parseGhTarget("Review the auth flow")).toBeUndefined();
    expect(parseGhTarget("")).toBeUndefined();
    expect(parseGhTarget("https://gitlab.com/foo/bar/issues/3")).toBeUndefined();
    expect(parseGhTarget("http://github.com/foo/bar/issues/3")).toBeUndefined();
    expect(parseGhTarget("https://github.com/foo/bar")).toBeUndefined();
    expect(parseGhTarget("https://github.com/foo/bar/issues/")).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// token provisioning: readGhToken / checkGhToken
// ---------------------------------------------------------------------------

describe("gh token file", () => {
  test("absent file → undefined / not-ok with a how-to reason", () => {
    const { ghDir } = fakeGhDir();
    expect(readGhToken()).toBeUndefined();
    const check = checkGhToken();
    expect(check.ok).toBe(false);
    expect(check.reason).toContain(join(ghDir, "ro-token.json"));
    expect(check.reason).toContain(`{ "token": "github_pat_…"`);
  });

  test("reads a valid token", () => {
    fakeGhDir({ token: "github_pat_test123" });
    expect(readGhToken()).toBe("github_pat_test123");
    expect(checkGhToken()).toEqual({ ok: true });
  });

  test("invalid JSON → loud, actionable error", () => {
    fakeGhDir({ raw: "not json {" });
    expect(() => readGhToken()).toThrow(/not valid JSON/);
    const check = checkGhToken();
    expect(check.ok).toBe(false);
    expect(check.reason).toContain("not valid JSON");
  });

  test("missing / empty `token` field → loud, actionable error", () => {
    fakeGhDir({ raw: "{}" });
    expect(() => readGhToken()).toThrow(/non-empty "token" field/);
    fakeGhDir({ raw: JSON.stringify({ token: "   " }) });
    expect(() => readGhToken()).toThrow(/non-empty "token" field/);
  });
});

// ---------------------------------------------------------------------------
// ghReviewGuard (onTransit)
// ---------------------------------------------------------------------------

describe("ghReviewGuard", () => {
  test("allows non-review targets untouched", async () => {
    fakeGhDir(); // no token — must not even be consulted
    const ctx = fakeTransit({ title: "no url here" });
    await expect(ghReviewGuard(ctx, { step: "cleanup" })).resolves.toBeUndefined();
    await expect(ghReviewGuard(ctx, { status: "done" })).resolves.toBeUndefined();
    expect(ctx.comments).toEqual([]);
  });

  test("allows a re-entry into review (visits > 0) without re-validating", async () => {
    fakeGhDir(); // no token — skipped for re-entries
    const ctx = fakeTransit({ title: "no url here", visits: 1 });
    await expect(ghReviewGuard(ctx, { step: "review" })).resolves.toBeUndefined();
    expect(ctx.comments).toEqual([]);
  });

  test("rejects the enroll when the token file is missing", async () => {
    const { ghDir } = fakeGhDir();
    const ctx = fakeTransit({ title: "https://github.com/wierdbytes/autosk/pull/12" });
    await expect(ghReviewGuard(ctx, { step: "review" })).rejects.toThrow(/no gh token file/);
    expect(ctx.comments).toHaveLength(1);
    expect(ctx.comments[0]).toContain(join(ghDir, "ro-token.json"));
  });

  test("rejects the enroll when the token file is unusable", async () => {
    fakeGhDir({ raw: "{{{" });
    const ctx = fakeTransit({ title: "https://github.com/wierdbytes/autosk/pull/12" });
    await expect(ghReviewGuard(ctx, { step: "review" })).rejects.toThrow(/not valid JSON/);
    expect(ctx.comments).toHaveLength(1);
  });

  test("rejects the enroll when no URL parses (title or description)", async () => {
    fakeGhDir({ token: "github_pat_test123" });
    const ctx = fakeTransit({ title: "Review the auth flow", description: "no link here either" });
    await expect(ghReviewGuard(ctx, { step: "review" })).rejects.toThrow(/no GitHub issue\/PR URL/);
    expect(ctx.comments).toHaveLength(1);
  });

  test("falls back to the description when the title has no URL", async () => {
    fakeGhDir({ token: "github_pat_test123" });
    const ctx = fakeTransit({
      title: "Review the auth PR",
      description: "see https://github.com/wierdbytes/autosk/pull/12",
    });
    await expect(ghReviewGuard(ctx, { step: "review" })).resolves.toBeUndefined();
    expect(ctx.comments).toEqual(["gh-review: reviewing PR wierdbytes/autosk#12."]);
  });

  test("comments the parsed issue target on a clean enroll", async () => {
    fakeGhDir({ token: "github_pat_test123" });
    const ctx = fakeTransit({ title: "https://github.com/wierdbytes/autosk/issues/9" });
    await expect(ghReviewGuard(ctx, { step: "review" })).resolves.toBeUndefined();
    expect(ctx.comments).toEqual(["gh-review: reviewing issue wierdbytes/autosk#9."]);
  });
});

// ---------------------------------------------------------------------------
// ghReviewSandbox — the docker run argv
// ---------------------------------------------------------------------------

describe("ghReviewSandbox", () => {
  test("wrap injects GH_TOKEN fresh from the token file, plus the no-write env", () => {
    setEnv("AUTOSK_GH_REVIEW_IMAGE", "gh-review-test-image");
    fakeGhDir({ token: "github_pat_test123" });
    const piDirPath = process.env.AUTOSK_PI_DIR as string;
    const projectRoot = mkTemp("ghrev-proj-");
    mkdirSync(join(projectRoot, ".git"));

    const sandbox = ghReviewSandbox();
    expect(sandbox.thin).toBe(true);
    const argv = sandbox.wrap(["pi", "--mode", "rpc"], {
      cwd: "/ws",
      env: {},
      id: { projectRoot, taskId: "ask-x1" },
    });

    expect(argv[0]).toBe("docker");
    // The token is passed as container ENV (gh's native mechanism) — read fresh
    // from the file at wrap time — with the no-write knobs alongside it.
    expect(argv).toContain("GH_TOKEN=github_pat_test123");
    expect(argv).toContain("GH_NO_UPDATE_NOTIFIER=1");
    expect(argv).toContain("CHECKPOINT_DISABLE=1");
    // There is NO gh config mount (gh must never see a config file to rewrite).
    expect(argv.join(" ")).not.toContain(".config/gh");
    // The pi config is mounted (rw) for the harness auth.
    expect(argv).toContain(`${piDirPath}:/home/agent/.pi`);
    // The project .git is layered at its identical host path, just before the image.
    const gitDir = join(projectRoot, ".git");
    const imgAt = argv.indexOf("gh-review-test-image");
    expect(imgAt).toBeGreaterThan(0);
    expect(argv[imgAt - 2]).toBe("-v");
    expect(argv[imgAt - 1]).toBe(`${gitDir}:${gitDir}`);
    // …followed by the harness command verbatim.
    expect(argv.slice(imgAt + 1)).toEqual(["pi", "--mode", "rpc"]);
  });

  test("wrap without a token file: no GH_TOKEN, no crash (the guard rejects the enroll)", () => {
    setEnv("AUTOSK_GH_REVIEW_IMAGE", "gh-review-test-image");
    fakeGhDir(); // no ro-token.json
    const sandbox = ghReviewSandbox();
    const argv = sandbox.wrap(["pi"], { cwd: "/ws", env: {}, id: { projectRoot: mkTemp("ghrev-nogit-"), taskId: "ask-x2" } });
    expect(argv.join(" ")).not.toContain("GH_TOKEN=");
    expect(argv).toContain("gh-review-test-image");
  });
});

// ---------------------------------------------------------------------------
// ghReviewWorkflow — the graph shape
// ---------------------------------------------------------------------------

describe("ghReviewWorkflow", () => {
  const fakeSandbox: Sandbox = {
    workspace: async () => ({ cwd: "/tmp" }),
    wrap: (cmd) => cmd,
    endpointFor: (port) => `http://127.0.0.1:${port}`,
    stop: async () => {},
    cleanup: async () => ({ removed: true, dirty: false }),
  };

  test("review → accept (human) → cleanup, with the enroll guard", () => {
    const wf = ghReviewWorkflow({ sandbox: fakeSandbox });
    expect(wf.name).toBe("gh-review");
    expect(wf.firstStep).toBe("review");
    expect(Object.keys(wf.steps).sort()).toEqual(["accept", "cleanup", "review"]);
    expect((wf.steps.accept as StatusStep).status).toBe("human");
    // The review step is an inline agent; cleanup is too (the teardown agent).
    expect(typeof (wf.steps.review as { onRun?: unknown }).onRun).toBe("function");
    expect(typeof (wf.steps.cleanup as { onRun?: unknown }).onRun).toBe("function");
    expect(wf.onTransit).toBe(ghReviewGuard);
  });
});
