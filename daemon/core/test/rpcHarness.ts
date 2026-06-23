/**
 * RPC test scaffolding (P5): spin up a real {@link startDaemon} over a temp UDS
 * with a temp `$HOME` (so it never touches the operator's `~/.autosk/`), plus a
 * tiny JSON-lines {@link RpcClient} that does request/response by id and collects
 * server→client notifications. Everything is drivable purely from `bun test`.
 */

import { mkdtempSync, rmSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  CapturingLogger,
  Engine,
  ProjectManager,
  ProjectRegistry,
  initProject,
  startDaemon,
  type DaemonRuntime,
  type EngineOptions,
  type ProjectHandle,
  type WatcherOptions,
} from "../src/index.ts";

/** The default explicit token so TCP auth tests are deterministic. */
export const TEST_TOKEN = "test-token-0123456789";

export interface TestDaemonOptions {
  /** Store watcher config (default: off). Pass an object to exercise external edits. */
  storeWatch?: WatcherOptions | false;
  engineOptions?: EngineOptions;
  tcp?: { host?: string; port: number } | null;
  token?: string | null;
  idleWindowMs?: number | null;
  idleCheckMs?: number;
  shutdownDelayMs?: number;
}

export interface TestDaemon {
  runtime: DaemonRuntime;
  socketPath: string;
  home: string;
  dir: string;
  token: string | null;
  projectManager: ProjectManager;
  logger: CapturingLogger;
  /** Resolves with the exit code once the daemon's shutdown path fires. */
  exited: Promise<number>;
  /** Creates an initialized project dir under the temp tree and returns its path. */
  makeProject(name: string): Promise<string>;
  /** The opened project handle for a cwd (for injecting test agents/workflows). */
  handle(cwd: string): Promise<ProjectHandle>;
  client(): Promise<RpcClient>;
  cleanup(): Promise<void>;
}

export async function startTestDaemon(opts: TestDaemonOptions = {}): Promise<TestDaemon> {
  const dir = mkdtempSync(join(tmpdir(), "autosk-rpc-"));
  const home = join(dir, "home");
  const socketPath = join(dir, "daemon.sock");
  const logger = new CapturingLogger();
  const registry = new ProjectRegistry(join(home, ".autosk", "projects.json"));
  const projectManager = new ProjectManager({
    registry,
    store: { watch: opts.storeWatch ?? false, logger },
    extensions: { home },
    logger,
  });

  let resolveExit!: (code: number) => void;
  const exited = new Promise<number>((r) => {
    resolveExit = r;
  });

  const result = await startDaemon({
    socketPath,
    token: opts.token === undefined ? TEST_TOKEN : opts.token,
    tcp: opts.tcp ?? null,
    projectManager,
    engine: new Engine({ rescanMs: 0, ...opts.engineOptions, logger }),
    idleWindowMs: opts.idleWindowMs ?? null,
    idleCheckMs: opts.idleCheckMs,
    shutdownDelayMs: opts.shutdownDelayMs,
    logger,
    exit: (code) => resolveExit(code),
  });
  if ("alreadyRunning" in result) throw new Error("startTestDaemon: unexpected alreadyRunning");

  const clients: RpcClient[] = [];
  return {
    runtime: result,
    socketPath,
    home,
    dir,
    token: result.token,
    projectManager,
    logger,
    exited,
    async makeProject(name: string): Promise<string> {
      const root = join(dir, name);
      await initProject(root);
      return root;
    },
    async handle(cwd: string): Promise<ProjectHandle> {
      return projectManager.resolve(cwd);
    },
    async client(): Promise<RpcClient> {
      const c = await RpcClient.connect(socketPath);
      clients.push(c);
      return c;
    },
    async cleanup(): Promise<void> {
      for (const c of clients) c.close();
      await result.shutdown();
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

/** A wire error surfaced as a thrown Error carrying the JSON-RPC `code`. */
export class RpcCallError extends Error {
  constructor(
    readonly code: number,
    message: string,
    readonly details?: unknown,
  ) {
    super(message);
    this.name = "RpcCallError";
  }
}

interface ResponseFrame {
  id: number;
  result?: unknown;
  error?: { code: number; message: string; details?: unknown };
}
interface NotificationFrame {
  method: string;
  params: unknown;
}

/** A minimal JSON-lines RPC client for tests (UDS or TCP). */
export class RpcClient {
  private buffer = "";
  private nextId = 1;
  private readonly pending = new Map<number, (frame: ResponseFrame) => void>();
  readonly notifications: NotificationFrame[] = [];
  /** Every response frame received, including unmatched ones (e.g. parse-error id 0). */
  readonly responseFrames: ResponseFrame[] = [];
  private readonly noteWaiters: { pred: (n: NotificationFrame) => boolean; resolve: (n: NotificationFrame) => void }[] = [];
  private readonly frameWaiters: { pred: (f: ResponseFrame) => boolean; resolve: (f: ResponseFrame) => void }[] = [];

  private constructor(private readonly socket: net.Socket) {
    socket.on("data", (chunk: Buffer) => this.feed(chunk));
    socket.on("error", () => {
      /* ignore; tests assert on absence of responses */
    });
  }

  static async connect(socketPath: string): Promise<RpcClient> {
    const socket = net.connect(socketPath);
    await new Promise<void>((resolve, reject) => {
      socket.once("connect", resolve);
      socket.once("error", reject);
    });
    return new RpcClient(socket);
  }

  static async connectTcp(host: string, port: number): Promise<RpcClient> {
    const socket = net.connect(port, host);
    await new Promise<void>((resolve, reject) => {
      socket.once("connect", resolve);
      socket.once("error", reject);
    });
    return new RpcClient(socket);
  }

  private feed(chunk: Buffer): void {
    this.buffer += chunk.toString("utf8");
    let nl: number;
    while ((nl = this.buffer.indexOf("\n")) >= 0) {
      const line = this.buffer.slice(0, nl);
      this.buffer = this.buffer.slice(nl + 1);
      if (line.trim().length === 0) continue;
      let frame: ResponseFrame | NotificationFrame;
      try {
        frame = JSON.parse(line);
      } catch {
        continue;
      }
      if (typeof (frame as NotificationFrame).method === "string") {
        const note = frame as NotificationFrame;
        this.notifications.push(note);
        for (let i = this.noteWaiters.length - 1; i >= 0; i--) {
          if (this.noteWaiters[i]!.pred(note)) this.noteWaiters.splice(i, 1)[0]!.resolve(note);
        }
      } else {
        const resp = frame as ResponseFrame;
        this.responseFrames.push(resp);
        const cb = this.pending.get(resp.id);
        if (cb) {
          this.pending.delete(resp.id);
          cb(resp);
        }
        for (let i = this.frameWaiters.length - 1; i >= 0; i--) {
          if (this.frameWaiters[i]!.pred(resp)) this.frameWaiters.splice(i, 1)[0]!.resolve(resp);
        }
      }
    }
  }

  /** Sends a request and resolves with the full response frame (never rejects on a wire error). */
  callRaw(method: string, params?: unknown): Promise<ResponseFrame> {
    const id = this.nextId++;
    return new Promise<ResponseFrame>((resolve) => {
      this.pending.set(id, resolve);
      this.socket.write(JSON.stringify({ id, method, params }) + "\n");
    });
  }

  /** Sends a request, returning the result or throwing {@link RpcCallError} on a wire error. */
  async call<T = unknown>(method: string, params?: unknown): Promise<T> {
    const frame = await this.callRaw(method, params);
    if (frame.error) throw new RpcCallError(frame.error.code, frame.error.message, frame.error.details);
    return frame.result as T;
  }

  /** Writes a raw line (for the malformed-line resilience test). */
  sendRawLine(line: string): void {
    this.socket.write(line + "\n");
  }

  waitForNotification(pred: (n: NotificationFrame) => boolean, timeoutMs = 2000): Promise<NotificationFrame> {
    const existing = this.notifications.find(pred);
    if (existing) return Promise.resolve(existing);
    return new Promise<NotificationFrame>((resolve, reject) => {
      const t = setTimeout(() => reject(new Error("waitForNotification: timed out")), timeoutMs);
      this.noteWaiters.push({
        pred,
        resolve: (n) => {
          clearTimeout(t);
          resolve(n);
        },
      });
    });
  }

  waitForFrame(pred: (f: ResponseFrame) => boolean, timeoutMs = 2000): Promise<ResponseFrame> {
    const existing = this.responseFrames.find(pred);
    if (existing) return Promise.resolve(existing);
    return new Promise<ResponseFrame>((resolve, reject) => {
      const t = setTimeout(() => reject(new Error("waitForFrame: timed out")), timeoutMs);
      this.frameWaiters.push({
        pred,
        resolve: (f) => {
          clearTimeout(t);
          resolve(f);
        },
      });
    });
  }

  close(): void {
    try {
      this.socket.destroy();
    } catch {
      /* already gone */
    }
  }
}
