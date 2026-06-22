/**
 * The per-session, host-side HTTP MCP server (plan §7) behind
 * `ctx.newMCPServer()`.
 *
 * A hand-rolled `Bun.serve()` Streamable-HTTP endpoint: `POST` → a single
 * JSON-RPC response as `application/json`, STATELESS (no `Mcp-Session-Id`), on
 * an EPHEMERAL loopback port (`port: 0`), gated by a per-server bearer token
 * (wrong/missing `Authorization` → 401). It reuses the transport-agnostic
 * {@link handleMessage} (the same dispatch the stdio server uses) with a DIRECT
 * tool backend (`directStoreBackend`) so `task` / `comment` hit the daemon's own
 * store with no `autosk` child. There is NO `@modelcontextprotocol/sdk`
 * dependency, so it survives `bun build --compile`.
 *
 * The agent gets back `{ url, port, token, close }`; it rewrites the host for its
 * isolation topology via `sandbox.endpointFor(port)` (e.g. `host.docker.internal`)
 * and hands the harness `Authorization: Bearer <token>`. `close()` is an explicit
 * early-release; the engine backstop closes the server on every settle / finaliser
 * / detach regardless.
 *
 * The socket binds `0.0.0.0` by default (not loopback): on Linux a container
 * reaching the host via `--add-host=host.docker.internal:host-gateway` connects
 * over the docker bridge IP, which a loopback-only bind does NOT listen on — so a
 * 127.0.0.1 bind refuses the in-container harness (the shipped `feature-dev-cc-docker`
 * path, and what the docker integration test must exercise). The security
 * boundary is the per-session 32-byte bearer + the ephemeral port (plan §7/§11.3),
 * NOT the bind interface; the returned `url` stays loopback for the host/worktree
 * case, and 0.0.0.0 still accepts those loopback connections.
 */

import { randomBytes } from "node:crypto";

import { handleMessage } from "./server.ts";
import type { McpToolBackend } from "./tools.ts";

/** A live per-session HTTP MCP server. */
export interface McpHttpServer {
  /**
   * Loopback URL for host-side use (`http://127.0.0.1:<port>`). The socket binds
   * all interfaces (so a container reaches it), but host/worktree callers use
   * this loopback form; a container agent rewrites only the host via
   * `sandbox.endpointFor(port)`.
   */
  url: string;
  /** The bound ephemeral port (the agent rewrites the host, keeps the port). */
  port: number;
  /** The bearer token a request must carry (`Authorization: Bearer <token>`). */
  token: string;
  /** Stops the server (idempotent). */
  close(): Promise<void>;
}

/** Options for {@link startMcpHttpServer}. */
export interface McpHttpServerOptions {
  /** The direct tool backend (`task` / `comment`). */
  backend: McpToolBackend;
  /** Advertise + dispatch the `transit` tool (task mode only). */
  transitEnabled: boolean;
  /** Bearer token to require (default: a fresh 32-byte hex token). */
  token?: string;
  /**
   * Interface to bind. Default `0.0.0.0` so a Linux container can reach the
   * server over the docker bridge (`host.docker.internal`); the bearer + ephemeral
   * port are the security gate. Tests may pin `127.0.0.1` to constrain the bind.
   */
  hostname?: string;
}

/** Minimal subset of `Bun.serve`'s returned server used here (avoids a hard Bun type dep). */
interface BunHttpServer {
  port: number;
  stop(closeActiveConnections?: boolean): void;
}

/**
 * Starts a per-session HTTP MCP server on an ephemeral loopback port. Returns
 * synchronously once bound (Bun binds eagerly), with the URL/port/token and a
 * `close()`.
 */
export function startMcpHttpServer(opts: McpHttpServerOptions): McpHttpServer {
  const token = opts.token ?? randomBytes(32).toString("hex");
  const expected = `Bearer ${token}`;

  const server = (
    Bun as unknown as { serve(o: Record<string, unknown>): BunHttpServer }
  ).serve({
    port: 0,
    // Bind all interfaces so an in-container harness reaches us over the docker
    // bridge (host.docker.internal); the bearer + ephemeral port are the gate.
    hostname: opts.hostname ?? "0.0.0.0",
    async fetch(req: Request): Promise<Response> {
      if (req.method !== "POST") {
        return new Response("method not allowed", { status: 405 });
      }
      // Per-request bearer: a wrong/missing token is a hard 401 (no tool runs).
      if ((req.headers.get("authorization") ?? "") !== expected) {
        return new Response("unauthorized", { status: 401 });
      }
      let msg: unknown;
      try {
        msg = await req.json();
      } catch {
        return Response.json(
          { jsonrpc: "2.0", id: null, error: { code: -32700, message: "parse error" } },
          { status: 200 },
        );
      }
      const resp = await handleMessage(msg as Parameters<typeof handleMessage>[0], {
        transitEnabled: opts.transitEnabled,
        backend: opts.backend,
      });
      // A notification (no id) yields null → 202 with no body.
      if (resp === null) return new Response(null, { status: 202 });
      return Response.json(resp, { status: 200 });
    },
  });

  const port = server.port;
  return {
    url: `http://127.0.0.1:${port}`,
    port,
    token,
    close: async (): Promise<void> => {
      server.stop(true);
    },
  };
}
