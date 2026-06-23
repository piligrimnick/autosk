/**
 * Transport seam for the autosk tools (plan: agent-owned isolation /
 * consolidated tool surface).
 *
 * `autosk_task` / `autosk_comment` work over TWO transports, chosen at call time
 * by {@link mcpEnabled}:
 *
 *   - **MCP** — when `AUTOSK_MCP_URL` is set (the autosk pi-agent injects it under
 *     a `dockerSandbox`/thin sandbox, the URL already rewritten to
 *     `host.docker.internal`), the tool POSTs a JSON-RPC `tools/call` to that
 *     per-session HTTP MCP server with the `AUTOSK_MCP_TOKEN` bearer. The server
 *     runs the action against the daemon's own store and returns the SAME
 *     `{ content, structuredContent }` shape the CLI path builds, so the renderers
 *     work unchanged. No `autosk` binary or mounted socket is needed in the
 *     container.
 *   - **CLI fallback** — otherwise (on the host, no MCP env) the tool shells out
 *     to `autosk … --json` via {@link runAutoskJson} (see `cli.ts`).
 *
 * This makes `@autosk/pi-tools` the single, transport-aware provider of the autosk
 * task/comment tools — the autosk pi-agent no longer injects a duplicate MCP
 * extension; it only injects the ack-only `autosk_transit` tool.
 */

import type { AutoskDetails, AutoskDomain, AutoskErrorReason } from "./types.ts";

/**
 * The tool result shape both transports return (matches the CLI path /
 * pi's `AgentToolResult<AutoskDetails | undefined>`: `details` is always present
 * but may be `undefined`).
 */
export interface ToolReturn {
	content: { type: "text"; text: string }[];
	details: AutoskDetails | undefined;
	isError: boolean;
}

/** The `McpToolResult` the daemon's HTTP MCP server returns for `task` / `comment`. */
interface McpToolResult {
	content: { type: "text"; text: string }[];
	isError?: boolean;
	structuredContent?: AutoskDetails;
}

/**
 * True when the per-session autosk MCP server is reachable (the autosk pi-agent
 * sets `AUTOSK_MCP_URL` under a thin sandbox). When false the tools fall back to
 * the `autosk` CLI.
 */
export function mcpEnabled(): boolean {
	const url = process.env.AUTOSK_MCP_URL;
	return typeof url === "string" && url.trim() !== "";
}

/**
 * POST a JSON-RPC `tools/call` (`name` = `"task"` | `"comment"`) to the
 * per-session HTTP MCP server with the bearer, and adapt the response into the
 * tool's {@link ToolReturn} shape. `args` is the tool's `{ action, args }`
 * envelope, forwarded verbatim (the server's schema mirrors the CLI tools').
 */
export async function mcpCall(
	domain: AutoskDomain,
	args: unknown,
	signal?: AbortSignal,
): Promise<ToolReturn> {
	const url = process.env.AUTOSK_MCP_URL ?? "";
	const token = process.env.AUTOSK_MCP_TOKEN ?? "";
	const action = readAction(args);
	if (url === "") return transportError(domain, action, "cli_error", "AUTOSK_MCP_URL is not set");

	let body: { result?: McpToolResult; error?: { message?: string } };
	try {
		const res = await fetch(url, {
			method: "POST",
			headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
			body: JSON.stringify({
				jsonrpc: "2.0",
				id: Date.now(),
				method: "tools/call",
				params: { name: domain, arguments: args },
			}),
			signal,
		});
		if (!res.ok) return transportError(domain, action, "cli_error", `MCP server returned HTTP ${res.status}`);
		body = (await res.json()) as { result?: McpToolResult; error?: { message?: string } };
	} catch (err) {
		if (err instanceof Error && err.name === "AbortError") {
			return transportError(domain, action, "aborted", "MCP request was aborted");
		}
		const message = err instanceof Error ? err.message : String(err);
		return transportError(domain, action, "cli_error", `MCP request failed: ${message}`);
	}

	if (body.error) return transportError(domain, action, "cli_error", body.error.message ?? "MCP error");
	const result = body.result;
	if (!result) return transportError(domain, action, "parse_error", "empty MCP response");
	return { content: result.content, details: result.structuredContent, isError: result.isError ?? false };
}

/** Best-effort `args.action` for error attribution. */
function readAction(args: unknown): string {
	const a = (args as { action?: unknown } | null)?.action;
	return typeof a === "string" ? a : "call";
}

/** Build the same `{ kind: "error" }` details shape the CLI path produces. */
function transportError(
	domain: AutoskDomain,
	action: string,
	reason: AutoskErrorReason,
	message: string,
): ToolReturn {
	const text = `autosk: ${message}`;
	return {
		content: [{ type: "text", text: `[${reason}] ${text}` }],
		details: { kind: "error", domain, action, reason, message: text },
		isError: true,
	};
}
