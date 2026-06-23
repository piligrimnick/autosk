/**
 * The `Sandbox` shape (plan §4) — **structural, not nominal**.
 *
 * `@autosk/sandbox` documents and exports this type so its own factories
 * (`worktreeSandbox()` / `dockerSandbox()`) and `sandboxCleanupStep()` can name
 * it, but agents type their `sandbox?` option STRUCTURALLY (an inline object
 * type) so a hand-rolled sandbox needs no dependency on this package. A sandbox
 * owns three things an agent delegates: the per-task workspace the harness runs
 * in (`workspace`), how to run the harness inside the sandbox (`wrap`), and how
 * an in-sandbox process reaches a host port (`endpointFor`); plus best-effort
 * teardown for abort (`stop`) and a cleanup step (`cleanup`).
 */

/** The deterministic identity of a task's sandbox env. */
export interface TaskIdentity {
  /** The canonical project root (the dir containing `.autosk/`). */
  projectRoot: string;
  /** The task id the env belongs to. */
  taskId: string;
}

/** The outcome of a {@link Sandbox.cleanup}. */
export interface SandboxCleanupResult {
  /** Whether an env existed for the identity and was removed. */
  removed: boolean;
  /** True when removal was SKIPPED because the env had uncommitted changes (and `force` was false). */
  dirty: boolean;
  /** Optional human-readable detail (e.g. `"3 uncommitted file(s)"`). */
  detail?: string;
}

/** Options the {@link Sandbox.wrap} seam resolves before building the argv. */
export interface SandboxWrapOptions {
  /** The workspace cwd the harness runs in (from {@link Sandbox.workspace}). */
  cwd: string;
  /** Env to inject into the in-sandbox process (e.g. container `-e KEY=VALUE`). */
  env?: Record<string, string>;
  /** The task identity (e.g. for the deterministic container name). */
  id: TaskIdentity;
  /**
   * Host files the harness needs available at the SAME path INSIDE the sandbox
   * (e.g. an injected pi extension passed via `pi -e <path>`). A container
   * sandbox bind-mounts each identical-path read-only so the in-container harness
   * resolves them; a host/worktree sandbox ignores them (the harness already sees
   * the host filesystem).
   */
  roFiles?: string[];
}

/**
 * A userspace sandbox an agent wraps its harness with. STRUCTURAL — agents
 * accept any object with these methods (no nominal import required).
 */
export interface Sandbox {
  /**
   * `true` when the sandbox runs the harness in a "thin" container with no
   * `autosk` CLI and no host filesystem visibility (e.g. `dockerSandbox` without
   * `mountSocket`). An agent that would otherwise shell out to `autosk`
   * (pi-agent's pi-tools path) must instead use the per-session HTTP MCP tool
   * surface, and pass any harness extension files via {@link SandboxWrapOptions.roFiles}
   * so they resolve inside the container. Absent/false for host/worktree
   * sandboxes (the harness runs on the host with `autosk` available). Claude-agent
   * always uses the HTTP MCP surface, so it ignores this flag.
   */
  thin?: boolean;
  /** Ensure the per-task workspace exists (idempotent + deterministic) and return the dir the harness runs in. */
  workspace(id: TaskIdentity): Promise<{ cwd: string }>;
  /** Wrap the harness argv to run inside the sandbox (e.g. `docker run …`). Identity for host/worktree. */
  wrap(cmd: string[], o: SandboxWrapOptions): string[];
  /** Isolation-correct host endpoint for an in-sandbox process to reach a host `port`. */
  endpointFor(port: number): string;
  /** Best-effort stop of the running sandbox process tree (agent `onAbort`). */
  stop(id: TaskIdentity): Promise<void>;
  /** Terminal teardown for the cleanup step: remove the workspace (+ any stray container). Idempotent. */
  cleanup(id: TaskIdentity, opts: { force: boolean }): Promise<SandboxCleanupResult>;
}
