/**
 * The JSON-lines transport (plan §4 / §3.7(5)).
 *
 * A thin accept layer over one or more `node:net` listeners (the single-instance
 * UDS from {@link listenUnix}, plus an opt-in TCP listener). Each accepted
 * socket becomes a {@link Connection} registered with the {@link Daemon}; the
 * connection owns its own read/write serialisation and the daemon owns dispatch.
 * The server only tracks open sockets so {@link close} can tear them all down on
 * shutdown.
 */

import type net from "node:net";

import { consoleLogger, type Logger } from "../store/index.ts";
import { Connection } from "./connection.ts";
import type { Daemon } from "./daemon.ts";

export interface ServeOptions {
  /** TCP connections require a `meta.auth` handshake; UDS is exempt. */
  isTcp: boolean;
  requireAuth: boolean;
}

export class RpcServer {
  private readonly sockets = new Set<net.Socket>();
  private readonly listeners: net.Server[] = [];
  private closed = false;

  constructor(
    private readonly daemon: Daemon,
    private readonly logger: Logger = consoleLogger,
  ) {}

  /** Begins accepting connections on `server` with the given transport options. */
  serve(server: net.Server, opts: ServeOptions): void {
    this.listeners.push(server);
    server.on("connection", (socket: net.Socket) => this.accept(socket, opts));
    server.on("error", (e: unknown) => {
      if (!this.closed) this.logger.error(`autoskd: accept error: ${e instanceof Error ? e.message : String(e)}`);
    });
  }

  private accept(socket: net.Socket, opts: ServeOptions): void {
    if (this.closed) {
      socket.destroy();
      return;
    }
    this.sockets.add(socket);
    socket.once("close", () => this.sockets.delete(socket));
    const conn = new Connection(socket, {
      dispatch: (method, params, c) => this.daemon.dispatch(method, params, c),
      onClose: (c) => this.daemon.removeConnection(c),
      isTcp: opts.isTcp,
      requireAuth: opts.requireAuth,
      logger: this.logger,
    });
    this.daemon.addConnection(conn);
  }

  /** Stops accepting and destroys every open connection. Idempotent. */
  close(): void {
    if (this.closed) return;
    this.closed = true;
    for (const listener of this.listeners) {
      try {
        listener.close();
      } catch {
        // already closing
      }
    }
    this.listeners.length = 0;
    for (const socket of this.sockets) {
      try {
        socket.destroy();
      } catch {
        // already gone
      }
    }
    this.sockets.clear();
  }
}
