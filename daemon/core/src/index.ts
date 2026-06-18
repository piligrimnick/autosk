/**
 * @autosk/core — the autoskd v2 daemon binary.
 *
 * Built across P2–P5:
 *  - P2: file store + project manager (this module's `store/` + `project/`);
 *  - P3: extension loader + per-project registries;
 *  - P4: scheduler + session lifecycle + `ctx.transit`;
 *  - P5: JSON-RPC v2 server (UDS + TCP/token, subscriptions, idle-shutdown).
 */

export * from "./store/index.ts";
export * from "./project/index.ts";
export * from "./extensions/index.ts";
export * from "./engine/index.ts";
export * from "./rpc/index.ts";
export * from "./mcp/index.ts";
export { VERSION, commit } from "./version.ts";

import { startDaemon } from "./rpc/index.ts";
import { runMcpServer } from "./mcp/index.ts";

/** Parsed `serve` flags. */
interface ServeArgs {
  sock?: string;
  tcp?: { host?: string; port: number };
  workers?: number;
}

function parseServeArgs(argv: string[]): ServeArgs {
  const out: ServeArgs = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]!;
    // For `--flag value` (space form), only consume the next token as the value
    // when it is not itself a flag — so `--tcp --workers 8` doesn't swallow
    // `--workers` as the TCP spec and silently drop the workers count (review #9).
    const take = (inline: string): string | undefined => {
      if (a.startsWith(`${inline}=`)) return a.slice(inline.length + 1);
      const next = argv[i + 1];
      if (next === undefined || next.startsWith("-")) return undefined;
      i++;
      return next;
    };
    if (a === "--sock" || a.startsWith("--sock=")) out.sock = take("--sock");
    else if (a === "--tcp" || a.startsWith("--tcp=")) out.tcp = parseTcp(take("--tcp"));
    else if (a === "--workers" || a.startsWith("--workers=")) {
      const v = Number.parseInt(take("--workers") ?? "", 10);
      if (Number.isInteger(v) && v > 0) out.workers = v;
    }
  }
  return out;
}

/** Parses a `HOST:PORT` (or bare `PORT`) TCP address. */
function parseTcp(spec: string | undefined): { host?: string; port: number } | undefined {
  if (!spec) return undefined;
  const idx = spec.lastIndexOf(":");
  if (idx < 0) {
    const port = Number.parseInt(spec, 10);
    return Number.isInteger(port) ? { port } : undefined;
  }
  const host = spec.slice(0, idx) || undefined;
  const port = Number.parseInt(spec.slice(idx + 1), 10);
  return Number.isInteger(port) ? { host, port } : undefined;
}

/** The daemon entrypoint: bind the single-instance UDS and serve proto-v2. */
export async function main(argv: string[] = process.argv.slice(2)): Promise<void> {
  // `mcp` is a self-contained, non-default verb: it runs the stdio MCP server
  // (the tool surface `@autosk/claude-agent` points Claude at) and never binds
  // the daemon socket. It returns when stdin closes.
  if (argv[0] === "mcp") {
    await runMcpServer();
    return;
  }

  // `serve` is the default verb; flags follow.
  const rest = argv[0] === "serve" ? argv.slice(1) : argv;
  const args = parseServeArgs(rest);

  const result = await startDaemon({
    socketPath: args.sock,
    tcp: args.tcp ?? null,
    engineOptions: args.workers !== undefined ? { workers: args.workers } : undefined,
  });

  if ("alreadyRunning" in result) {
    // Single-instance: another daemon won the bind. The connecting client uses
    // it; exit cleanly so a double-spawn is harmless.
    console.error("autoskd: already running");
    process.exit(0);
  }

  for (const sig of ["SIGINT", "SIGTERM"] as const) {
    process.once(sig, () => void result.shutdown());
  }
  // The UDS listener keeps the event loop alive; serve() returns immediately.
}

if (import.meta.main) {
  // A daemon entrypoint must fail loud-but-clean: startDaemon() rethrows any
  // non-AlreadyRunning failure (mkdir/bind/chmod, an HOME-unset resolve throw),
  // so surface a one-line message + non-zero exit instead of an unhandled
  // promise rejection (review #1).
  main().catch((e) => {
    console.error(`autoskd: ${e instanceof Error ? e.message : String(e)}`);
    process.exit(1);
  });
}
