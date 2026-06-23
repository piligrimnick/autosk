/**
 * The per-session HTTP MCP server (`ctx.newMCPServer()`) + the direct-store tool
 * backend (plan §7).
 *
 *  - POST → a single JSON-RPC response as `application/json`, stateless,
 *    ephemeral port; a valid bearer lists tools + runs calls, a wrong/missing
 *    bearer is a hard 401;
 *  - `task` / `comment` run DIRECT against the store (real writes) and return the
 *    same `McpToolResult` shape the shell-out path does;
 *  - per-session binding: `transit` advertised only in task mode (ack-only),
 *    `comment` default author = the bound step, explicit ids honoured.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import { directStoreBackend, startMcpHttpServer, Store, type McpHttpServer } from "../src/index.ts";
import { tempDir } from "./helpers.ts";

describe("HTTP MCP server + direct-store backend", () => {
  let dir: ReturnType<typeof tempDir>;
  let store: Store;
  let server: McpHttpServer;

  beforeEach(async () => {
    dir = tempDir();
    store = new Store(dir.path, { watch: false });
    await store.open();
    server = startMcpHttpServer({
      backend: directStoreBackend({
        store,
        author: "dev",
        // No engine in this unit: a create-with-workflow would call enroll, which
        // these tests do not exercise.
        enroll: async () => {
          throw new Error("enroll not wired in this test");
        },
      }),
      transitEnabled: true,
    });
  });
  afterEach(async () => {
    await server.close();
    await store.close();
    dir.cleanup();
  });

  /** POSTs a JSON-RPC message with the given (or the valid) bearer. */
  async function post(body: unknown, token = server.token): Promise<Response> {
    return fetch(server.url, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
      body: JSON.stringify(body),
    });
  }

  async function call(name: string, args: Record<string, unknown>): Promise<{ content: { text: string }[]; isError: boolean; structuredContent?: unknown }> {
    const res = await post({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name, arguments: args } });
    const body = (await res.json()) as { result: { content: { text: string }[]; isError: boolean; structuredContent?: unknown } };
    return body.result;
  }

  test("url is loopback and carries the bound port", () => {
    expect(server.url).toBe(`http://127.0.0.1:${server.port}`);
    expect(server.port).toBeGreaterThan(0);
    expect(server.token.length).toBeGreaterThan(0);
  });

  test("a wrong/missing bearer is a hard 401 (no tool runs)", async () => {
    const noAuth = await fetch(server.url, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }),
    });
    expect(noAuth.status).toBe(401);
    const wrong = await post({ jsonrpc: "2.0", id: 1, method: "tools/list" }, "nope");
    expect(wrong.status).toBe(401);
  });

  test("a non-POST is 405", async () => {
    const res = await fetch(server.url, { method: "GET", headers: { authorization: `Bearer ${server.token}` } });
    expect(res.status).toBe(405);
  });

  test("tools/list advertises transit (task mode) + task + comment as application/json", async () => {
    const res = await post({ jsonrpc: "2.0", id: 2, method: "tools/list" });
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("application/json");
    const body = (await res.json()) as { result: { tools: { name: string }[] } };
    expect(body.result.tools.map((t) => t.name)).toEqual(["transit", "task", "comment"]);
  });

  test("transit is ack-only (does not perform a transition)", async () => {
    const r = await call("transit", { to: "review" });
    expect(r.isError).toBe(false);
    expect(r.content[0]!.text).toContain('transition to "review" submitted');
  });

  test("comment add writes to the store with the bound step as default author; list reads it back", async () => {
    const task = await store.createTask({ title: "T" });
    const added = await call("comment", { action: "add", args: { task_id: task.id, text: "from dev" } });
    expect(added.isError).toBe(false);
    const detail = added.structuredContent as { kind: string; comment: { author: string; text: string } };
    expect(detail.kind).toBe("comment");
    expect(detail.comment.author).toBe("dev"); // bound step is the default author
    expect(detail.comment.text).toBe("from dev");

    // A real store write happened.
    const comments = await store.listComments(task.id);
    expect(comments.map((c) => c.text)).toEqual(["from dev"]);

    // An explicit author override is honoured.
    const byAlice = await call("comment", { action: "add", args: { task_id: task.id, text: "n", author: "alice" } });
    expect((byAlice.structuredContent as { comment: { author: string } }).comment.author).toBe("alice");
  });

  test("task create/show/list run direct against the store with the --json wire shape", async () => {
    const created = await call("task", { action: "create", args: { title: "Build auth", description: "login" } });
    const cd = created.structuredContent as { kind: string; task: { id: string; title: string; status: string; workflow: string; blocked: boolean; comment_count: number } };
    expect(cd.kind).toBe("task");
    expect(cd.task.title).toBe("Build auth");
    expect(cd.task.status).toBe("new");
    expect(cd.task.workflow).toBe(""); // never-enrolled → "" (not null) on the wire
    expect(cd.task.blocked).toBe(false);
    expect(cd.task.comment_count).toBe(0);

    const shown = await call("task", { action: "show", args: { id: cd.task.id } });
    expect((shown.structuredContent as { task: { id: string } }).task.id).toBe(cd.task.id);

    const listed = await call("task", { action: "list" });
    const ld = listed.structuredContent as { kind: string; tasks: { id: string }[] };
    expect(ld.kind).toBe("tasks");
    expect(ld.tasks.some((t) => t.id === cd.task.id)).toBe(true);
  });

  test("task show with a missing id returns invalid_args without touching the store", async () => {
    const r = await call("task", { action: "show", args: {} });
    expect(r.isError).toBe(true);
    expect((r.structuredContent as { reason: string }).reason).toBe("invalid_args");
  });
});

describe("HTTP MCP server — interactive (transit disabled)", () => {
  test("tools/list omits transit when transitEnabled is false", async () => {
    const dir = tempDir();
    const store = new Store(dir.path, { watch: false });
    await store.open();
    const server = startMcpHttpServer({
      backend: directStoreBackend({ store, author: "@autosk/pi-agent", enroll: async () => { throw new Error("no enroll"); } }),
      transitEnabled: false,
    });
    try {
      const res = await fetch(server.url, {
        method: "POST",
        headers: { "content-type": "application/json", authorization: `Bearer ${server.token}` },
        body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }),
      });
      const body = (await res.json()) as { result: { tools: { name: string }[] } };
      expect(body.result.tools.map((t) => t.name)).toEqual(["task", "comment"]);
    } finally {
      await server.close();
      await store.close();
      dir.cleanup();
    }
  });
});
