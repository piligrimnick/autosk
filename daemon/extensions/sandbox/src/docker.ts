/**
 * `dockerSandbox()` — run the harness inside a per-task `docker run -i --rm`
 * container (plan §4, §8).
 *
 * Replaces the retired `@autosk/docker` `dockerIsolation()` provider. The whole
 * keep-alive container + `docker exec` seam + RUNNING/DORMANT state machine is
 * gone: a container now lives exactly as long as ONE harness process. The agent
 * spawns the wrapped argv (`docker run …`) directly, and `onAbort` → `stop()` →
 * `docker stop <name>` covers a SIGKILL orphan.
 *
 *  - `workspace()` composes {@link worktreeSandbox} — the per-task git worktree
 *    on the host (branch `autosk/<task-id>`), bind-mounted 1:1 into the container;
 *  - `wrap()` = `docker run -i --rm --name <det> --add-host=host.docker.internal:host-gateway
 *    -v <ws>:<ws> -w <ws> -e … <image> <cmd…>`;
 *  - `endpointFor()` = `http://host.docker.internal:<port>` so an in-container
 *    harness reaches the host MCP server;
 *  - `stop()` = `docker stop <name>`;
 *  - `cleanup()` = the worktree cleanup PLUS a defensive `docker rm -f <name>`
 *    (a `--rm` container self-removes on exit; this only catches a daemon-crash
 *    orphan).
 */

import { homedir } from "node:os";
import { existsSync, mkdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

import type { Sandbox, SandboxCleanupResult, SandboxWrapOptions, TaskIdentity } from "./types.ts";
import { canonRoot, slugFor, worktreeSandbox } from "./worktree.ts";

/** A first-class bind mount injected into the container (plan §8). */
export interface DockerMount {
  /** Host path (a leading `~` is expanded to the operator HOME). */
  hostPath: string;
  /** Path inside the container. */
  sandboxPath: string;
  /** Mount read-only (`:ro`). */
  readonly?: boolean;
}

/** Options for {@link dockerSandbox}. */
export interface DockerSandboxOptions {
  /**
   * Operator image with the harness (`claude`/`pi`) + build/test toolchain
   * preinstalled. REQUIRED — the sandbox just `docker run`s it; there is no build
   * step, no `socat`, no `autosk`/`autoskd`, no mounted socket.
   */
  image: string;
  /** `docker` binary to shell out to. Defaults to `"docker"` (honours podman/nerdctl). */
  dockerBin?: string;
  /** Container env baked at `docker run` as `-e KEY=VALUE` (e.g. `ANTHROPIC_API_KEY`). */
  env?: Record<string, string>;
  /** First-class bind mounts (e.g. `~/.claude/.credentials.json:ro`, caches). */
  mounts?: DockerMount[];
  /**
   * `--user` value (e.g. `"1000:1000"`) so the bind-mounted worktree/creds stay
   * host-owned. Defaults to the daemon's own uid:gid; pass `""` to omit `--user`.
   */
  user?: string;
  /** Container HOME (e.g. `/home/agent`) so `~/.claude` mounts land where the harness looks. */
  home?: string;
  /** Extra `docker run` args (e.g. `--network`, `--cpus`, `--memory`). */
  runArgs?: string[];
  /**
   * ESCAPE HATCH (pi fallback, plan §5.2): bind-mount the daemon UDS + set
   * `AUTOSK_SOCK` so an in-container `autosk` can reach the host daemon. Default
   * `false` — both shipped agents use the host http MCP server instead.
   */
  mountSocket?: boolean;
  /** Daemon UDS path for {@link mountSocket}. Default `$AUTOSK_SOCK` → `<home>/.autosk/daemon.sock`. */
  socketPath?: string;
  /** Host home for the inner worktree (`<worktreeHome>/.autosk/worktrees/…`). Test injection. */
  worktreeHome?: string;
  /** git binary for the inner worktree. */
  gitBin?: string;
}

/**
 * Builds a per-task docker {@link Sandbox}. Pass it to an agent step
 * (`dev: claudeAgent({ sandbox: dockerSandbox({ image }) })`) and wire a
 * `sandboxCleanupStep(sandbox)` into the workflow's terminal path.
 */
export function dockerSandbox(opts: DockerSandboxOptions): Sandbox {
  if (!opts.image || opts.image.trim() === "") {
    throw new Error("dockerSandbox: `image` is required");
  }
  const dockerBin = opts.dockerBin ?? "docker";
  // Compose the worktree workspace (host FS; branch autosk/<id>) ONCE so
  // `workspace` and `cleanup` share its git-available cache.
  const fs = worktreeSandbox({ home: opts.worktreeHome, gitBin: opts.gitBin });

  return {
    // A docker container is a THIN image (no `autosk` CLI, no host FS) UNLESS the
    // operator opted into the `mountSocket` escape hatch (image ships `autosk`,
    // the daemon UDS is mounted) — then the harness uses the CLI over the socket
    // instead of the host HTTP MCP surface. pi-agent reads this to pick its tool
    // surface; claude-agent always uses HTTP and ignores it.
    thin: !opts.mountSocket,

    workspace: (id: TaskIdentity) => fs.workspace(id),

    wrap: (cmd: string[], o: SandboxWrapOptions): string[] =>
      runArgv(dockerBin, containerName(o.id.projectRoot, o.id.taskId), o.cwd, cmd, o.env, opts, o.roFiles),

    endpointFor: (port: number): string => `http://host.docker.internal:${port}`,

    async stop({ projectRoot, taskId }: TaskIdentity): Promise<void> {
      const name = containerName(projectRoot, taskId);
      await ensureDockerAvailable(dockerBin);
      // Best-effort: tolerate "already stopped / gone".
      await runDocker(dockerBin, ["stop", name]);
    },

    async cleanup(id: TaskIdentity, { force }): Promise<SandboxCleanupResult> {
      // Dirty gate delegates to the worktree notion of dirty. With force=false a
      // dirty checkout is LEFT in place (removed:false, dirty:true) — and we leave
      // the container too so the operator can inspect it.
      const inner = await fs.cleanup(id, { force });
      if (inner.dirty && !inner.removed) return inner;
      // Clean, or forced: the worktree is gone (branch preserved) — destroy any
      // orphan container too. A `--rm` container normally self-removed on exit;
      // `docker rm -f` returns nonzero when it is already gone (idempotent).
      await ensureDockerAvailable(dockerBin);
      const name = containerName(id.projectRoot, id.taskId);
      const rm = await runDocker(dockerBin, ["rm", "-f", name]);
      return { ...inner, removed: inner.removed || rm.code === 0 };
    },
  };
}

// ---------------------------------------------------------------------------
// Deterministic container identity (byte-identical to @autosk/docker).
// ---------------------------------------------------------------------------

/**
 * The deterministic container name for `(projectRoot, taskId)`:
 * `autosk-<slugFor(canonRoot)>-<taskId>`, reusing the SAME slug the inner
 * worktree uses (so `wrap`, `stop`, and `cleanup` all resolve the same container
 * with no in-memory state). Sanitised to the docker container-name charset
 * (`[a-zA-Z0-9][a-zA-Z0-9_.-]*`).
 */
export function containerName(projectRoot: string, taskId: string): string {
  if (projectRoot.trim() === "") throw new Error("project root is empty");
  if (taskId.trim() === "") throw new Error("task id is empty");
  return sanitizeName(`autosk-${slugFor(canonRoot(projectRoot))}-${taskId}`);
}

/**
 * Coerces a name into docker's container-name charset. Replaces any disallowed
 * char with `_`, then ensures the first char is alphanumeric. Deterministic, so
 * wrap/stop/cleanup still resolve the same name.
 */
function sanitizeName(name: string): string {
  const cleaned = name.replace(/[^a-zA-Z0-9_.-]/g, "_");
  return /^[a-zA-Z0-9]/.test(cleaned) ? cleaned : `a${cleaned}`;
}

// ---------------------------------------------------------------------------
// docker run argv construction.
// ---------------------------------------------------------------------------

/**
 * The `docker run` argv for one harness process: `-i` (pipe stdin so the agent
 * streams the harness' stdio), `--rm` (self-remove on exit), `--name <det>`,
 * `--add-host=host.docker.internal:host-gateway` (so the in-container harness
 * reaches the host MCP server), the worktree bind-mounted 1:1 (`-v <ws>:<ws>`)
 * as the `-w` workdir (zero path translation), a `-e KEY=VALUE` per env entry,
 * the operator mounts, the identical-path read-only `roFiles` (injected harness
 * extension files), `--user` / HOME / run args, the image, and the command.
 */
export function runArgv(
  dockerBin: string,
  name: string,
  cwd: string,
  cmd: string[],
  env: Record<string, string> | undefined,
  opts: DockerSandboxOptions,
  roFiles: string[] = [],
): string[] {
  const args = [
    dockerBin,
    "run",
    "-i",
    "--rm",
    "--name",
    name,
    "--add-host=host.docker.internal:host-gateway",
    // Identical-path bind mount: the worktree is a valid -w workdir, no translation.
    "-v",
    `${cwd}:${cwd}`,
    "-w",
    cwd,
  ];
  const user = opts.user ?? defaultUser();
  if (user) args.push("--user", user);
  if (opts.home) args.push("-e", `HOME=${opts.home}`);
  if (opts.mountSocket) {
    const sock = resolveSocketPath(opts.socketPath, opts.worktreeHome);
    args.push("-v", `${sock}:${sock}`, "-e", `AUTOSK_SOCK=${sock}`);
  }
  for (const m of opts.mounts ?? []) {
    const host = expandHome(m.hostPath, opts.home);
    ensureMountParent(host);
    args.push("-v", `${host}:${m.sandboxPath}${m.readonly ? ":ro" : ""}`);
  }
  // Identical-path read-only mounts for harness files the agent injects by host
  // path (e.g. `pi -e <ext>`): the same path is valid inside the container, so
  // `pi` resolves it with zero translation (the worktree `-v ws:ws` trick).
  for (const f of roFiles) {
    args.push("-v", `${f}:${f}:ro`);
  }
  // Construction env first, then the per-run env (per-run wins on a collision).
  for (const [k, v] of Object.entries({ ...(opts.env ?? {}), ...(env ?? {}) })) {
    args.push("-e", `${k}=${v}`);
  }
  args.push(...(opts.runArgs ?? []));
  args.push(opts.image, ...cmd);
  return args;
}

/** The daemon's own `uid:gid` so bind-mounted files stay host-owned, or `""` when unavailable. */
function defaultUser(): string {
  const uid = typeof process.getuid === "function" ? process.getuid() : undefined;
  const gid = typeof process.getgid === "function" ? process.getgid() : undefined;
  return uid !== undefined && gid !== undefined ? `${uid}:${gid}` : "";
}

/** Expands a leading `~` (or `~/…`) to the container HOME (when set) else the host HOME. */
function expandHome(p: string, home?: string): string {
  if (p === "~") return home ?? homedir();
  if (p.startsWith("~/")) return join(home ?? homedir(), p.slice(2));
  return p;
}

/** Best-effort: create the parent dir of a single-file bind mount that does not exist yet. */
function ensureMountParent(host: string): void {
  if (existsSync(host)) return;
  try {
    mkdirSync(dirname(resolve(host)), { recursive: true });
  } catch {
    /* best-effort; docker surfaces a clear error if the source is unusable */
  }
}

/** Resolves the daemon UDS path: explicit override → `$AUTOSK_SOCK` → `<home>/.autosk/daemon.sock`. */
export function resolveSocketPath(override?: string, home?: string): string {
  if (override && override.length > 0) return override;
  const env = process.env.AUTOSK_SOCK;
  if (env && env.length > 0) return env;
  const h = home ?? process.env.HOME ?? homedir();
  if (!h || h.length === 0) throw new Error("user home dir not set; cannot resolve the daemon socket");
  return join(h, ".autosk", "daemon.sock");
}

// ---------------------------------------------------------------------------
// docker verbs.
// ---------------------------------------------------------------------------

interface DockerResult {
  code: number | null;
  stdout: string;
  stderr: string;
}

/** Runs `<dockerBin> <args...>`, capturing stdout/stderr/exit code. */
async function runDocker(dockerBin: string, args: string[]): Promise<DockerResult> {
  const proc = Bun.spawn([dockerBin, ...args], { stdin: "ignore", stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, code] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  return { code, stdout, stderr };
}

/**
 * Caches a SUCCESSFUL `<dockerBin> version` per binary (a failure is re-checked,
 * never cached). Keyed by `dockerBin` so a second sandbox built with a different
 * binary (podman/nerdctl) is validated independently.
 */
const dockerAvailableByBin = new Map<string, boolean>();
async function ensureDockerAvailable(dockerBin: string): Promise<void> {
  if (dockerAvailableByBin.get(dockerBin) === true) return;
  let ok = false;
  try {
    const proc = Bun.spawn([dockerBin, "version", "--format", "{{.Server.Version}}"], {
      stdout: "ignore",
      stderr: "ignore",
      stdin: "ignore",
    });
    ok = (await proc.exited) === 0;
  } catch {
    ok = false;
  }
  if (ok) dockerAvailableByBin.set(dockerBin, true);
  else throw new Error(`docker daemon not reachable (\`${dockerBin} version\` failed)`);
}
