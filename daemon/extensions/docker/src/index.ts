/**
 * `@autosk/docker` — the opt-in `dockerIsolation()` provider (plan
 * `docs/plans/20260618-Docker-Isolation.md`).
 *
 * A FULL SANDBOX: pi (and every command it spawns) runs INSIDE a per-task Docker
 * container via `docker exec`, while the git-branch / review-merge story is
 * preserved by COMPOSING `@autosk/worktree` for the filesystem. The provider:
 *
 *  - delegates the FS lifecycle to an inner {@link IsolationProvider} (default
 *    {@link worktreeIsolation}) — a per-task git worktree on branch
 *    `autosk/<task-id>`, edits landing on the host;
 *  - bind-mounts that worktree into the container at the SAME absolute path
 *    (`-v <wt.cwd>:<wt.cwd>`), so `ctx.cwd` is a valid `-w` workdir inside the
 *    container with ZERO path translation in the exec/spawn seam;
 *  - bind-mounts the daemon UDS (and sets `AUTOSK_SOCK`) so the in-container
 *    `autosk` reaches the host daemon for full `@autosk/pi-tools` parity
 *    (`autosk_comment` / `autosk_task` / transit);
 *  - returns an {@link IsolationHandle} whose `exec`/`spawn` rewrite the argv to
 *    `docker exec -i -w <cwd> -e <env…> <container> <cmd…>` — the engine routes
 *    `ctx.exec` / `ctx.spawn` through it (plan §5), so the agent stays
 *    isolation-agnostic.
 *
 * Lifecycle (mirrors the worktree state machine, plus container moves):
 *  - **acquire** = ensure-ready: inner.acquire (worktree) → `docker inspect` →
 *    `run` (absent) / `start` (dormant) / reuse (running). Idempotent and
 *    recovery-safe (deterministic container name → no in-memory state).
 *  - **release** = quiesce-on-exit: `docker stop` (keep the container DORMANT,
 *    cheap to `docker start` on resume).
 *  - **reap** = destroy-on-terminal: dirty gate via the inner worktree reap, then
 *    `docker rm -f` + the inner worktree removal (branch preserved). Keyed by
 *    identity, idempotent, recovery-safe.
 *
 * FAILURES ARE WRAPPED BY THE ENGINE (`acquire` throw → `isolation_acquire_failed`,
 * `reap` throw → `isolation_reap_failed`), so this provider just throws
 * descriptive messages — it never parks or formats those prefixes itself.
 */

import { homedir } from "node:os";
import { join, resolve } from "node:path";
import { realpathSync } from "node:fs";

import {
  runChild,
  spawnChild,
  type ChildHandle,
  type ExecResult,
  type IsolationExecOptions,
  type IsolationHandle,
  type IsolationProvider,
  type IsolationReapResult,
  type IsolationSpawnOptions,
} from "@autosk/sdk";
import { slugFor, worktreeIsolation } from "@autosk/worktree";

/** Options for {@link dockerIsolation}. */
export interface DockerIsolationOptions {
  /**
   * Operator image with `pi` (and a container-compatible `autosk`) preinstalled.
   * REQUIRED — the provider just `docker run`s it; there is no build step.
   */
  image: string;
  /** `docker` binary to shell out to. Defaults to `"docker"` (honours podman/nerdctl). */
  dockerBin?: string;
  /**
   * Filesystem provider to compose for the per-task workspace. Defaults to
   * {@link worktreeIsolation} (built with `home`/`gitBin`).
   */
  inner?: IsolationProvider;
  /** Bind-mount the daemon UDS into the container. Default `true`. */
  mountSocket?: boolean;
  /** Daemon UDS path. Default `$AUTOSK_SOCK` → `<home>/.autosk/daemon.sock`. */
  socketPath?: string;
  /**
   * Host `autosk` binary to bind-mount at `/usr/local/bin/autosk:ro` (for a
   * cross-arch host where the image's `autosk` would not match — see README).
   */
  autoskBin?: string;
  /** Extra `docker run` args (e.g. `--network`, `--cpus`, `--memory`, `--user`). */
  runArgs?: string[];
  /** Extra container env baked at `docker run` (inherited by every `docker exec`). */
  env?: Record<string, string>;
  /** Forwarded to the default inner {@link worktreeIsolation} (test injection). */
  home?: string;
  /** Forwarded to the default inner {@link worktreeIsolation}. */
  gitBin?: string;
}

/** The provider tag the registry / `workflow.get` renders for a docker-isolated workflow. */
export const DOCKER_TAG = "docker";

/**
 * Keep-alive entrypoint so the container stays hot across steps. `tail -f
 * /dev/null` is portable across BOTH GNU coreutils and BusyBox/Alpine — unlike
 * `sleep infinity`, which BusyBox `sleep` rejects (`invalid number 'infinity'`),
 * making the container die instantly on small operator images. We only ever
 * drive the container via `docker exec`, so the entrypoint just has to keep PID 1
 * alive.
 */
const KEEPALIVE_ENTRYPOINT = "tail";
const KEEPALIVE_ARGS = ["-f", "/dev/null"];

/** Provider-internal bookkeeping carried on every {@link IsolationHandle}. */
interface DockerMeta extends Record<string, unknown> {
  /** The deterministic container name (`autosk-<slug>-<task>`). */
  container: string;
}

/**
 * Builds the opt-in docker {@link IsolationProvider}. Attach it to a workflow via
 * `isolation: dockerIsolation({ image: "my-org/autosk-runtime:latest" })`. The
 * engine calls `acquire` before scheduling each session (its returned `cwd`
 * becomes `ctx.cwd`, and its `exec`/`spawn` become `ctx.exec`/`ctx.spawn`),
 * `release` when the task leaves `work`, and `reap` on a terminal transition.
 */
export function dockerIsolation(opts: DockerIsolationOptions): IsolationProvider {
  if (!opts.image || opts.image.trim() === "") {
    throw new Error("dockerIsolation: `image` is required");
  }
  const dockerBin = opts.dockerBin ?? "docker";
  const mountSocket = opts.mountSocket ?? true;
  // Build the inner FS provider ONCE so `acquire` and `reap` share it (and its
  // git-available cache). Composing the shipped worktree provider gives us the
  // git-branch isolation; a test may inject a different `inner`.
  const inner = opts.inner ?? worktreeIsolation({ home: opts.home, gitBin: opts.gitBin });

  return {
    tag: DOCKER_TAG,

    async acquire({ projectRoot, taskId }): Promise<IsolationHandle> {
      // 1. Filesystem: the per-task git worktree on the host (branch autosk/<id>).
      const wt = await inner.acquire({ projectRoot, taskId });
      const name = containerName(projectRoot, taskId);
      const meta: DockerMeta = { ...wt.meta, container: name };

      // 2. Ensure the container is RUNNING (create | start | reuse).
      await ensureDockerAvailable(dockerBin);
      const state = await inspectState(dockerBin, name);
      if (state === "running") {
        // reuse — already hot.
      } else if (state === "stopped") {
        const r = await runDocker(dockerBin, ["start", name]);
        if (r.code !== 0) throw new Error(`docker start ${name}: ${dockerErr(r)}`);
        await assertRunning(dockerBin, name, "start");
      } else {
        const r = await runDocker(dockerBin, runArgsFor(name, wt.cwd, opts, mountSocket));
        if (r.code !== 0) throw new Error(`docker run ${opts.image}: ${dockerErr(r)}`);
        await assertRunning(dockerBin, name, "run");
      }

      // 3. Hand back a handle whose exec/spawn run INSIDE the container.
      return {
        cwd: wt.cwd,
        meta,
        exec: (cmd: string[], o: IsolationExecOptions): Promise<ExecResult> =>
          // Forward `input`/`timeoutMs` so the docker seam honours the SAME
          // `ExecOptions` fields the host path does (engine/child.ts):
          //  - `input`: `docker exec -i` already pipes stdin, so the bytes flow
          //    host client → container process (e.g. `git apply` patch on stdin);
          //  - `timeoutMs`: kills the host `docker exec` client and resolves the
          //    same non-zero ExecResult the host path returns (the in-container
          //    process may orphan — known §10 caveat, backstopped by
          //    `release`→`docker stop`).
          runChild(dockerExecArgv(dockerBin, name, cmd, o), {
            signal: o.signal,
            input: o.input,
            timeoutMs: o.timeoutMs,
          }),
        spawn: (cmd: string[], o: IsolationSpawnOptions): ChildHandle =>
          spawnChild(dockerExecArgv(dockerBin, name, cmd, o), { signal: o.signal }),
      };
    },

    async release(handle): Promise<void> {
      // Quiesce-on-exit: stop the container but KEEP it (DORMANT → cheap restart).
      const name = (handle.meta as DockerMeta | undefined)?.container;
      if (!name) return; // nothing we can address
      await ensureDockerAvailable(dockerBin);
      // Best-effort: tolerate "already stopped / gone".
      await runDocker(dockerBin, ["stop", name]);
    },

    async reap({ projectRoot, taskId }, { force }): Promise<IsolationReapResult> {
      const name = containerName(projectRoot, taskId);
      // Dirty gate delegates to the worktree notion of dirty. With force=false a
      // dirty checkout is LEFT in place (removed:false, dirty:true) — and we leave
      // the container too, so the operator can inspect it.
      const innerResult = inner.reap
        ? await inner.reap({ projectRoot, taskId }, { force })
        : ({ removed: false, dirty: false } satisfies IsolationReapResult);
      if (innerResult.dirty && !innerResult.removed) {
        return innerResult; // refused (force=false): keep worktree AND container
      }
      // Clean, or forced: the worktree is gone (branch preserved) — destroy the
      // container too. `docker rm -f` returns nonzero when the container is
      // already gone (idempotent re-reap), so OR its success into `removed` —
      // that way even a seam-less custom inner with no FS artifact still reports
      // the container teardown (rather than a misleading `removed: false`).
      await ensureDockerAvailable(dockerBin);
      const rm = await runDocker(dockerBin, ["rm", "-f", name]);
      return { ...innerResult, removed: innerResult.removed || rm.code === 0 };
    },
  };
}

// ---------------------------------------------------------------------------
// Deterministic container identity.
// ---------------------------------------------------------------------------

/**
 * The deterministic container name for `(projectRoot, taskId)`:
 * `autosk-<slugFor(canonRoot)>-<taskId>`, reusing the SAME slug the inner
 * worktree uses (so `acquire` re-use/restart, `reap` by identity, and crash
 * recovery all resolve the same container with no in-memory state). Sanitised to
 * the docker container-name charset (`[a-zA-Z0-9][a-zA-Z0-9_.-]*`).
 */
export function containerName(projectRoot: string, taskId: string): string {
  if (projectRoot.trim() === "") throw new Error("project root is empty");
  if (taskId.trim() === "") throw new Error("task id is empty");
  return sanitizeName(`autosk-${slugFor(canonRoot(projectRoot))}-${taskId}`);
}

/** Symlink-resolved, absolutised project root (matches the worktree `canonRoot`). */
function canonRoot(projectRoot: string): string {
  const abs = resolve(projectRoot);
  try {
    return realpathSync(abs);
  } catch {
    return abs; // lexical-clean fallback when the path can't be canonicalised
  }
}

/**
 * Coerces a name into docker's container-name charset. Replaces any disallowed
 * char with `_`, then ensures the first char is alphanumeric. Deterministic, so
 * acquire/reap/recovery still resolve the same name.
 */
function sanitizeName(name: string): string {
  const cleaned = name.replace(/[^a-zA-Z0-9_.-]/g, "_");
  return /^[a-zA-Z0-9]/.test(cleaned) ? cleaned : `a${cleaned}`;
}

// ---------------------------------------------------------------------------
// docker argv construction.
// ---------------------------------------------------------------------------

/**
 * The `docker exec` argv for a command run INSIDE the container: `-i` (pipe
 * stdin, so `ctx.spawn` can stream into pi's stdio), `-w <cwd>` (the 1:1
 * bind-mounted worktree), and a `-e KEY=VALUE` for each env entry (carries
 * pi-agent's `AUTOSK_CWD` / `AUTOSK_AGENT` so the in-container `autosk` targets
 * the right project and attributes comments correctly).
 */
export function dockerExecArgv(
  dockerBin: string,
  name: string,
  cmd: string[],
  o: { cwd: string; env?: Record<string, string> },
): string[] {
  const envFlags = Object.entries(o.env ?? {}).flatMap(([k, v]) => ["-e", `${k}=${v}`]);
  return [dockerBin, "exec", "-i", "-w", o.cwd, ...envFlags, name, ...cmd];
}

/**
 * The `docker run` argv that creates the per-task container: detached, named,
 * with the worktree bind-mounted 1:1, the daemon UDS mounted (+ `AUTOSK_SOCK`),
 * an optional cross-arch `autosk` bind-mount, the operator env / run args, the
 * image, and a portable keep-alive entrypoint (`tail -f /dev/null`) so the
 * container stays hot across steps (we only ever drive it via `docker exec`).
 */
export function runArgsFor(
  name: string,
  wtCwd: string,
  opts: DockerIsolationOptions,
  mountSocket: boolean,
): string[] {
  const args = ["run", "-d", "--name", name];
  // Identical-path bind mount: ctx.cwd is a valid -w workdir with no translation.
  args.push("-v", `${wtCwd}:${wtCwd}`);
  if (mountSocket) {
    const sock = resolveSocketPath(opts.socketPath, opts.home);
    args.push("-v", `${sock}:${sock}`, "-e", `AUTOSK_SOCK=${sock}`);
  }
  if (opts.autoskBin) {
    args.push("-v", `${opts.autoskBin}:/usr/local/bin/autosk:ro`);
  }
  for (const [k, v] of Object.entries(opts.env ?? {})) args.push("-e", `${k}=${v}`);
  args.push(...(opts.runArgs ?? []));
  // Override the image's entrypoint with a keep-alive so the container stays up
  // independent of what the image normally runs; we only use it via `docker exec`.
  args.push("--entrypoint", KEEPALIVE_ENTRYPOINT, opts.image, ...KEEPALIVE_ARGS);
  return args;
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

/**
 * The container's lifecycle state: `running` (reuse), `stopped` (DORMANT →
 * start), or `absent` (create). `docker inspect -f '{{.State.Running}}'` prints
 * `true`/`false`, or fails (nonzero) when the container does not exist.
 */
async function inspectState(dockerBin: string, name: string): Promise<"running" | "stopped" | "absent"> {
  const r = await runDocker(dockerBin, ["inspect", "-f", "{{.State.Running}}", name]);
  if (r.code !== 0) return "absent";
  return r.stdout.trim() === "true" ? "running" : "stopped";
}

/**
 * Verifies the container actually STAYED running after `docker run` / `docker
 * start`. `docker run -d` exits 0 as soon as the container is *created and
 * started*, even if its entrypoint then dies immediately (e.g. a non-portable
 * keep-alive on a BusyBox/Alpine image). Without this gate a bad image would
 * surface as a confusing "container is not running" on the FIRST `docker exec`
 * instead of a clear acquire failure; best-effort `docker logs` is appended so
 * the operator sees why PID 1 exited.
 */
async function assertRunning(dockerBin: string, name: string, verb: string): Promise<void> {
  if ((await inspectState(dockerBin, name)) === "running") return;
  const logs = await runDocker(dockerBin, ["logs", "--tail", "10", name]);
  const detail = `${logs.stdout}${logs.stderr}`.trim();
  throw new Error(
    `docker ${verb} ${name}: container is not running — its keep-alive command (PID 1) exited immediately` +
      (detail ? `; container logs: ${detail}` : "") +
      ". The image's entrypoint must keep PID 1 alive; the provider runs `tail -f /dev/null` " +
      "(portable across coreutils and BusyBox).",
  );
}

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

function dockerErr(r: DockerResult): string {
  const msg = `${r.stdout}${r.stderr}`.trim();
  return msg === "" ? `exit ${r.code}` : msg;
}

/**
 * Caches a SUCCESSFUL `<dockerBin> version` per binary (a failure is re-checked,
 * never cached). Keyed by `dockerBin` so a second provider built with a
 * different binary (podman/nerdctl) is validated independently.
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
