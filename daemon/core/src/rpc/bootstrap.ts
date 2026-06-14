/**
 * Daemon bootstrap (plan §4 / §3.7(5)): bind the single-instance UDS, wire the
 * {@link Engine} + {@link ProjectManager} into a {@link Daemon}, serve over UDS
 * (+ opt-in TCP/token), and install the shutdown path used by `meta.shutdown`
 * and the idle-shutdown watchdog.
 *
 * {@link startDaemon} is the seam the binary's `main()` and the tests share: a
 * loser of the single-instance race returns `{ alreadyRunning: true }` (the
 * binary then exits 0), and an `exit` override lets tests observe shutdown
 * without killing the test process.
 */

import net from "node:net";

import { Engine, type EngineOptions } from "../engine/index.ts";
import { ProjectManager } from "../project/index.ts";
import { consoleLogger, type Logger } from "../store/index.ts";
import { Daemon } from "./daemon.ts";
import { resolveIdleWindowMs, resolveSocketPath } from "./paths.ts";
import { RpcServer } from "./server.ts";
import { ensureToken, resolveTokenPath } from "./token.ts";
import { AlreadyRunningError, listenUnix, type UnixListenHandle } from "./uds.ts";
import { VERSION } from "../version.ts";

export interface StartDaemonOptions {
  /** UDS path override; else `$AUTOSK_SOCK` → `~/.autosk/daemon.sock`. */
  socketPath?: string;
  /** Token file path: `undefined` ⇒ resolve from env/home; `null` ⇒ no token file. */
  tokenPath?: string | null;
  /** Explicit token override (tests) — wins over `tokenPath`. */
  token?: string | null;
  /** Enable the opt-in TCP transport (token auth). */
  tcp?: { host?: string; port: number } | null;
  projectManager?: ProjectManager;
  engine?: Engine;
  engineOptions?: EngineOptions;
  /** Idle-shutdown window in ms: `undefined` ⇒ `$AUTOSK_IDLE_SECS`; disabled in TCP mode. */
  idleWindowMs?: number | null;
  idleCheckMs?: number;
  /** Delay before the shutdown hook runs, so a `meta.shutdown` reply flushes first. */
  shutdownDelayMs?: number;
  /** Process-exit override (default `process.exit`); tests pass a recorder. */
  exit?: (code: number) => void;
  logger?: Logger;
}

export interface DaemonRuntime {
  daemon: Daemon;
  server: RpcServer;
  socketPath: string;
  token: string | null;
  tcpAddress?: { host: string; port: number };
  /** Tears everything down (server + engine + stores) and removes the socket. */
  shutdown(): Promise<void>;
}

export type StartDaemonResult = DaemonRuntime | { alreadyRunning: true };

/** Binds + serves the daemon, or returns `{ alreadyRunning }` if another won the bind. */
export async function startDaemon(opts: StartDaemonOptions = {}): Promise<StartDaemonResult> {
  const logger = opts.logger ?? consoleLogger;
  const socketPath = resolveSocketPath(opts.socketPath);

  let unix: UnixListenHandle;
  try {
    unix = await listenUnix(socketPath);
  } catch (e) {
    if (e instanceof AlreadyRunningError) return { alreadyRunning: true };
    throw e;
  }

  const token = resolveDaemonToken(opts, logger);
  // There are no daemon-bundled extensions: on first run (no
  // `~/.autosk/settings.json`) the default project manager npm-installs the
  // reference `@autosk/feature-dev` workflow into `~/.autosk/packages/` and
  // writes `settings.json`, so every project then discovers it through the
  // normal npm-packages source. Tests inject their own `projectManager` (with no
  // `bootstrap`), so this default branch — and any real `npm install` — is never
  // hit by the suite.
  const projectManager = opts.projectManager ?? new ProjectManager({ logger, bootstrap: {} });
  const engine = opts.engine ?? new Engine({ ...opts.engineOptions, logger });

  // Idle-shutdown is disabled in TCP mode (a remote daemon is a long-lived service).
  let idleWindowMs = opts.idleWindowMs !== undefined ? opts.idleWindowMs : resolveIdleWindowMs();
  if (opts.tcp) idleWindowMs = null;

  const daemon = new Daemon({
    projectManager,
    engine,
    token,
    logger,
    idleWindowMs,
    idleCheckMs: opts.idleCheckMs,
    shutdownDelayMs: opts.shutdownDelayMs,
  });
  const server = new RpcServer(daemon, logger);

  let tcpListener: net.Server | null = null;
  let shuttingDown = false;
  const shutdown = async (): Promise<void> => {
    if (shuttingDown) return;
    shuttingDown = true;
    server.close();
    if (tcpListener) {
      try {
        tcpListener.close();
      } catch {
        // already closing
      }
    }
    await daemon.close();
    unix.release();
    (opts.exit ?? ((code) => process.exit(code)))(0);
  };
  daemon.onShutdownRequested(() => void shutdown());
  daemon.start();

  // Attach the connection handler BEFORE the socket starts accepting: a
  // net.Server drops "connection" events emitted before a listener exists, so
  // listening first would silently strand any client that connects in the gap
  // (concurrent first-callers hung on a request the daemon never read).
  server.serve(unix.server, { isTcp: false, requireAuth: false });
  await unix.listen();

  // Kick off the first-run bootstrap AFTER the socket is accepting, so the Go
  // auto-spawn readiness wait is never blocked by an `npm install`. A project
  // open awaits the same single-flight promise, so `feature-dev` is guaranteed
  // present by the time it is used even if this warm-up has not finished.
  void projectManager.ensureBootstrap();

  let tcpAddress: { host: string; port: number } | undefined;
  if (opts.tcp) {
    const host = opts.tcp.host ?? "127.0.0.1";
    try {
      const listener = net.createServer();
      tcpListener = listener;
      // Same ordering rule as the UDS above: wire the handler, then listen.
      server.serve(listener, { isTcp: true, requireAuth: true });
      await new Promise<void>((resolve, reject) => {
        const onError = (e: unknown) => reject(e);
        listener.once("error", onError);
        listener.listen(opts.tcp!.port, host, () => {
          listener.removeListener("error", onError);
          resolve();
        });
      });
      const addr = listener.address();
      tcpAddress =
        addr && typeof addr === "object" ? { host: addr.address, port: addr.port } : { host, port: opts.tcp.port };
      logger.info(`autoskd: TCP listening on ${tcpAddress.host}:${tcpAddress.port} (token auth)`);
    } catch (e) {
      // A TCP bind failure (e.g. port in use) is non-fatal: the UDS is already
      // serving, so log and continue UDS-only (mirrors the v1 daemon).
      logger.error(`autoskd: bind tcp ${host}:${opts.tcp.port}: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  logger.info(`autoskd ${VERSION}: listening on ${socketPath}`);
  return { daemon, server, socketPath, token, tcpAddress, shutdown };
}

/** Resolves the configured token: explicit override, else the token file (or `null`). */
function resolveDaemonToken(opts: StartDaemonOptions, logger: Logger): string | null {
  if (opts.token !== undefined) return opts.token;
  const tokenPath = opts.tokenPath === undefined ? resolveTokenPath() : opts.tokenPath;
  if (!tokenPath) return null;
  try {
    return ensureToken(tokenPath);
  } catch (e) {
    logger.error(`autoskd: token: ${e instanceof Error ? e.message : String(e)}`);
    return null;
  }
}
