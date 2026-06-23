/**
 * The JSON-RPC v2 server (plan §4 / §3.7(5)): JSON-lines over UDS (+ opt-in
 * TCP/token), single-instance bind, subscriptions/replay, and idle-shutdown.
 */

export { Daemon, type DaemonOptions } from "./daemon.ts";
export { RpcServer, type ServeOptions } from "./server.ts";
export { Connection, SessionSubscription, type DispatchFn, type ConnectionDeps } from "./connection.ts";
export { RpcError, toRpcError } from "./errors.ts";
export {
  startDaemon,
  type StartDaemonOptions,
  type StartDaemonResult,
  type DaemonRuntime,
} from "./bootstrap.ts";
export { listenUnix, AlreadyRunningError, type UnixListenHandle } from "./uds.ts";
export { ensureToken, resolveTokenPath } from "./token.ts";
export { resolveSocketPath, resolveIdleWindowMs, DEFAULT_IDLE_SECS } from "./paths.ts";
