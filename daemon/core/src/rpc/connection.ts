/**
 * One client connection over the JSON-lines transport (plan §4 / §3.7(5)).
 *
 * Owns: a per-connection READ chain (requests handled one at a time, in order),
 * a per-connection WRITE chain (responses + pushed notifications serialised so
 * frames never interleave), the TCP auth gate, and the connection's
 * subscription state (`task-changed` roots, the `project-changed` flag, and live
 * `session.subscribe` cursors). The {@link Daemon} reaches into the subscription
 * fields directly when it fans an event out.
 */

import type net from "node:net";

import { ErrorCodes, type SessionEventParams, type SessionMeta } from "@autosk/sdk";

import type { Logger } from "../store/logger.ts";
import type { SessionStore } from "../store/sessionStore.ts";
import { toRpcError } from "./errors.ts";

/** Dispatches one method for a connection. Throws to signal a wire error. */
export type DispatchFn = (method: string, params: unknown, conn: Connection) => Promise<unknown>;

export interface ConnectionDeps {
  dispatch: DispatchFn;
  /** Called exactly once when the connection is torn down (either side). */
  onClose: (conn: Connection) => void;
  isTcp: boolean;
  /** When true (TCP with a token), every connection must `meta.auth` first. */
  requireAuth: boolean;
  logger: Logger;
}

/** The engine `session-event` slice a {@link SessionSubscription} reacts to. */
export interface SessionEventInput {
  kind: "status" | "done" | "error" | "message";
  meta?: SessionMeta;
  error?: string;
}

export class Connection {
  /** Authenticated? UDS starts true; TCP-with-token starts false until `meta.auth`. */
  authed: boolean;
  /** Roots this connection wants `task-changed` for (via `task.subscribe`). */
  readonly taskRoots = new Set<string>();
  /** Whether this connection wants `project-changed` (via `project.subscribe`). */
  wantsProject = false;
  /** Live `session.subscribe` cursors, keyed by session id. */
  readonly sessionSubs = new Map<string, SessionSubscription>();

  private buffer = "";
  private writeChain: Promise<void> = Promise.resolve();
  private reqChain: Promise<void> = Promise.resolve();
  private closed = false;
  private cleaned = false;

  constructor(
    private readonly socket: net.Socket,
    private readonly deps: ConnectionDeps,
  ) {
    this.authed = !deps.requireAuth;
    socket.setNoDelay?.(true);
    socket.on("data", (chunk: Buffer) => this.feed(chunk));
    socket.once("close", () => this.cleanup());
    socket.on("error", () => {
      // A transport error is always followed by 'close'; swallow it so an
      // ECONNRESET can never crash the daemon.
    });
  }

  get isTcp(): boolean {
    return this.deps.isTcp;
  }

  // -- read path -----------------------------------------------------------

  private feed(chunk: Buffer): void {
    this.buffer += chunk.toString("utf8");
    let nl: number;
    while ((nl = this.buffer.indexOf("\n")) >= 0) {
      const line = this.buffer.slice(0, nl);
      this.buffer = this.buffer.slice(nl + 1);
      this.enqueueRequest(line);
    }
  }

  /** Requests are processed in arrival order, one at a time (per connection). */
  private enqueueRequest(line: string): void {
    this.reqChain = this.reqChain.then(() => this.processLine(line));
  }

  private async processLine(line: string): Promise<void> {
    if (this.closed) return;
    if (line.trim().length === 0) return;

    let req: { id?: unknown; method?: unknown; params?: unknown };
    try {
      req = JSON.parse(line);
    } catch (e) {
      // A malformed line must NOT kill the connection (plan §4 error handling).
      this.send({
        id: 0,
        error: { code: ErrorCodes.PARSE_ERROR, message: `parse error: ${e instanceof Error ? e.message : String(e)}` },
      });
      return;
    }
    if (typeof req !== "object" || req === null) {
      this.send({ id: 0, error: { code: ErrorCodes.INVALID_REQUEST, message: "request must be a JSON object" } });
      return;
    }
    const id = typeof req.id === "number" ? req.id : 0;
    const method = req.method;
    if (typeof method !== "string" || method.length === 0) {
      this.send({ id, error: { code: ErrorCodes.INVALID_REQUEST, message: "missing method" } });
      return;
    }
    // TCP auth handshake: until authenticated, only `meta.auth` is served.
    if (this.deps.requireAuth && !this.authed && method !== "meta.auth") {
      this.send({ id, error: { code: ErrorCodes.INVALID_REQUEST, message: "auth required" } });
      return;
    }
    try {
      const result = await this.deps.dispatch(method, req.params, this);
      this.send({ id, result: result ?? null });
    } catch (e) {
      this.send({ id, error: toRpcError(e) });
    }
  }

  // -- write path ----------------------------------------------------------

  /** Serialised write of one JSON object as a single newline-terminated frame. */
  send(obj: unknown): void {
    if (this.closed) return;
    const text = JSON.stringify(obj) + "\n";
    this.writeChain = this.writeChain
      .then(
        () =>
          new Promise<void>((resolve) => {
            if (this.closed) return resolve();
            this.socket.write(text, () => resolve());
          }),
      )
      .catch(() => {
        // a write failure means the peer is gone; 'close' handles teardown
      });
  }

  // -- teardown ------------------------------------------------------------

  /** Server-initiated close (idle-shutdown / explicit). Destroys the socket. */
  close(): void {
    this.closed = true;
    try {
      this.socket.destroy();
    } catch {
      // already gone
    }
    this.cleanup();
  }

  /** Fires {@link ConnectionDeps.onClose} exactly once (both teardown paths). */
  private cleanup(): void {
    if (this.cleaned) return;
    this.cleaned = true;
    this.closed = true;
    this.deps.onClose(this);
  }
}

/**
 * A live `session.subscribe` cursor (plan §4: replay-then-tail). On
 * {@link start} it replays transcript lines from `from_line` then emits the
 * current status; thereafter every engine `session-event` for this session
 * drives {@link onEvent}, which pumps any newly-appended transcript lines (so
 * line numbers stay monotonic and gap-free) and forwards status/done/error
 * frames. All work is serialised on a per-subscription chain so two events can
 * never interleave their reads.
 */
export class SessionSubscription {
  private cursor: number;
  private chain: Promise<void> = Promise.resolve();

  constructor(
    readonly root: string,
    readonly sessionId: string,
    private readonly sessions: SessionStore,
    private readonly conn: Connection,
    fromLine: number | undefined,
  ) {
    this.cursor = Math.max(1, fromLine ?? 1);
  }

  /** Replays from the cursor, then emits the current status (+ done/error if terminal). */
  start(): void {
    this.enqueue(async () => {
      await this.clampCursor();
      await this.pump();
      const meta = await this.sessions.getMeta(this.sessionId);
      if (!meta) return;
      this.frame("status", meta);
      if (meta.status === "done") this.frame("done", meta);
      else if (meta.status === "failed" || meta.status === "aborted") this.frame("error", meta);
    });
  }

  /** Reacts to one engine `session-event`: pump new lines, then forward the frame. */
  onEvent(ev: SessionEventInput): void {
    this.enqueue(async () => {
      await this.pump();
      if (ev.kind === "message") return;
      const meta = ev.meta ?? (await this.sessions.getMeta(this.sessionId)) ?? undefined;
      if (!meta) return;
      this.frame(ev.kind, meta);
    });
  }

  /**
   * Clamps an out-of-range `from_line` down to the current tail (review #7).
   * `readTranscript` reports `nextLine == cursor` for BOTH "cursor exactly at the
   * tail" and "cursor past the tail", so without this a `from_line` beyond the
   * end parks the cursor above every later append and silently drops them. A
   * full read here yields the true end (`nextLine == lineCount + 1`); pump()
   * thereafter only ever advances the cursor to an in-range `nextLine`, so this
   * one clamp at start is sufficient.
   */
  private async clampCursor(): Promise<void> {
    const { nextLine } = await this.sessions.readTranscript(this.sessionId, { fromLine: 1 });
    if (this.cursor > nextLine) this.cursor = nextLine;
  }

  private enqueue(fn: () => Promise<void>): void {
    this.chain = this.chain.then(fn).catch(() => {
      // a read/write failure here must not poison later events
    });
  }

  /** Emits every transcript line at/after the cursor as `message` frames. */
  private async pump(): Promise<void> {
    const { lines, nextLine } = await this.sessions.readTranscript(this.sessionId, { fromLine: this.cursor });
    let lineNo = this.cursor;
    for (const line of lines) {
      const params: SessionEventParams = {
        root: this.root,
        session_id: this.sessionId,
        kind: "message",
        event: line,
        line: lineNo,
      };
      this.conn.send({ method: "session-event", params });
      lineNo++;
    }
    this.cursor = nextLine;
  }

  private frame(kind: "status" | "done" | "error", meta: SessionMeta): void {
    const params: SessionEventParams = {
      root: this.root,
      session_id: this.sessionId,
      kind,
      session: meta,
    };
    if (kind === "error" && meta.error !== undefined) params.error = meta.error;
    this.conn.send({ method: "session-event", params });
  }
}
