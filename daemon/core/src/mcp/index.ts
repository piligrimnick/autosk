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
} from "./server.ts";
export { callTask, callComment, callTransit, TASK_TOOL, COMMENT_TOOL, TRANSIT_TOOL, type McpTool, type McpToolResult } from "./tools.ts";
export { AutoskCliError, bunRunProcess, type RunProcess, type RunResult, type RunOptions } from "./cli.ts";
export * from "./types.ts";
