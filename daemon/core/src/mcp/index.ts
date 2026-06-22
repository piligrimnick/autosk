/**
 * `@autosk/core` — the `autoskd mcp` stdio MCP server (the self-contained tool
 * surface `@autosk/claude-agent` points Claude Code at via `--mcp-config`).
 */

export {
  runMcpServer,
  handleMessage,
  listTools,
  callTool,
  transitEnabledFromEnv,
  type McpServerOptions,
  type McpDispatchOptions,
} from "./server.ts";
export {
  callTask,
  callComment,
  callTransit,
  shellBackend,
  TASK_TOOL,
  COMMENT_TOOL,
  TRANSIT_TOOL,
  type McpTool,
  type McpToolResult,
  type McpToolBackend,
  type McpActionParams,
} from "./tools.ts";
export { directStoreBackend, type StoreBackendBinding } from "./store-backend.ts";
export { startMcpHttpServer, type McpHttpServer, type McpHttpServerOptions } from "./http.ts";
export { AutoskCliError, bunRunProcess, type RunProcess, type RunResult, type RunOptions } from "./cli.ts";
export * from "./types.ts";
