/**
 * `@autosk/docker` tests (Docker-Isolation plan §11):
 *
 *  - UNIT (no docker): deterministic `containerName` (reuses the inner worktree
 *    slug), the `dockerExecArgv` rewrite, the `docker run` argv, socket-path
 *    resolution, and the `image`-required guard.
 *  - LIFECYCLE (hermetic, fake `docker` shim + fake inner): acquire
 *    create/start/reuse sequencing, `release` → stop, `reap` → inner dirty-gate
 *    then `rm -f`, and idempotency.
 *  - COMPOSITION (gated on git, fake `docker` shim + the REAL worktree inner):
 *    branch created on acquire; worktree gone + branch preserved on reap.
 *  - INTEGRATION (gated on a real `docker version` probe + a small image): the
 *    exec/spawn seam actually runs commands INSIDE the container (`pwd` == the
 *    bind-mounted worktree path; a `-e` var is visible), `release` stops it, and
 *    `reap` removes container + worktree while preserving the branch.
 */

import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import { chmodSync, existsSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { IsolationHandle, IsolationProvider, IsolationReapResult } from "@autosk/sdk";
import { branchFor, pathFor, slugFor } from "@autosk/worktree";

import { containerName, dockerExecArgv, dockerIsolation, resolveSocketPath, runArgsFor } from "../src/index.ts";

// ---------------------------------------------------------------------------
// temp / git / docker probes.
// ---------------------------------------------------------------------------

const temps: string[] = [];
function mkTemp(prefix: string): string {
  const d = realpathSync(mkdtempSync(join(tmpdir(), prefix)));
  temps.push(d);
  return d;
}
afterEach(() => {
  for (const d of temps.splice(0)) rmSync(d, { recursive: true, force: true });
});

let gitOk = false;
let dockerOk = false;
const TEST_IMAGE = "busybox:latest";
let imageOk = false;

beforeAll(() => {
  gitOk = Bun.spawnSync(["git", "--version"]).exitCode === 0;
  dockerOk = Bun.spawnSync(["docker", "version", "--format", "{{.Server.Version}}"]).exitCode === 0;
  if (dockerOk) {
    const present = Bun.spawnSync(["docker", "image", "inspect", TEST_IMAGE]).exitCode === 0;
    imageOk = present || Bun.spawnSync(["docker", "pull", TEST_IMAGE]).exitCode === 0;
  }
});

function git(args: string[], cwd: string): { code: number; stdout: string } {
  const p = Bun.spawnSync(["git", ...args], {
    cwd,
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "t",
      GIT_AUTHOR_EMAIL: "t@e",
      GIT_COMMITTER_NAME: "t",
      GIT_COMMITTER_EMAIL: "t@e",
    },
  });
  return { code: p.exitCode, stdout: p.stdout.toString() };
}

/** A fresh git repo with one commit; returns its (realpath'd) root. */
function makeRepo(): string {
  const root = mkTemp("dk-repo-");
  git(["init", "-q", "-b", "main"], root);
  writeFileSync(join(root, "README"), "x");
  git(["add", "."], root);
  git(["commit", "-q", "-m", "init"], root);
  return root;
}

function branchListed(root: string, branch: string): boolean {
  return git(["show-ref", "--verify", "--quiet", `refs/heads/${branch}`], root).code === 0;
}

// ---------------------------------------------------------------------------
// a hermetic fake `docker` CLI (state on disk + a command log).
// ---------------------------------------------------------------------------

/**
 * A per-test fake `docker` CLI. The state dir + command log are EMBEDDED in the
 * generated shim script (not passed via env): `Bun.spawn` inherits only the
 * daemon's STARTUP env, so a runtime `process.env` mutation would never reach
 * the shim. POSIX-sh behaviour: `version` ok; `inspect` reads a per-name state
 * file (true|false, or exit 1 when absent); run/start write "true"; stop writes
 * "false"; rm removes the file and exits nonzero when already absent (matching
 * real `docker rm -f`, so reap can OR the teardown into its result). Every
 * invocation is appended to the log.
 */
function withFakeDocker(opts: { ephemeral?: boolean } = {}): { dockerBin: string; stateDir: string; lines(): string[] } {
  const dir = mkTemp("dk-shim-");
  const stateDir = join(dir, "state");
  const logFile = join(dir, "log");
  Bun.spawnSync(["mkdir", "-p", stateDir]);
  writeFileSync(logFile, "");
  const shimPath = join(dir, "fake-docker");
  // `ephemeral` simulates a container whose keep-alive (PID 1) dies immediately:
  // run/start still exit 0 (as real `docker run -d` does) but never persist a
  // "running" state, so the post-run `inspect` reads absent → acquire must fail.
  const runCase = opts.ephemeral
    ? `run) printf 'fakeid\\n'; exit 0 ;;`
    : `run) printf 'true' > "$STATE/$(san "$name")"; printf 'fakeid\\n'; exit 0 ;;`;
  const startCase = opts.ephemeral
    ? `start) exit 0 ;;`
    : `start) printf 'true' > "$STATE/$(san "$last")"; exit 0 ;;`;
  writeFileSync(
    shimPath,
    `#!/bin/sh
STATE="${stateDir}"
LOG="${logFile}"
printf '%s\\n' "$*" >> "$LOG"
cmd="$1"; shift
last=""; for a in "$@"; do last="$a"; done
name=""; prev=""; for a in "$@"; do if [ "$prev" = "--name" ]; then name="$a"; fi; prev="$a"; done
san() { printf '%s' "$1" | tr -c 'a-zA-Z0-9_.-' '_'; }
case "$cmd" in
  version) exit 0 ;;
  inspect) f="$STATE/$(san "$last")"; [ -f "$f" ] || exit 1; cat "$f"; exit 0 ;;
  ${runCase}
  ${startCase}
  stop) f="$STATE/$(san "$last")"; [ -f "$f" ] && printf 'false' > "$f"; exit 0 ;;
  rm) f="$STATE/$(san "$last")"; if [ -f "$f" ]; then rm -f "$f"; exit 0; else exit 1; fi ;;
  *) exit 0 ;;
esac
`,
  );
  chmodSync(shimPath, 0o755);
  return {
    dockerBin: shimPath,
    stateDir,
    lines: () =>
      readFileSync(logFile, "utf8")
        .split("\n")
        .map((l) => l.trim())
        .filter((l) => l !== ""),
  };
}

/** A fake inner FS provider that records reap calls and returns a canned result. */
function fakeInner(opts: { reapResult?: IsolationReapResult } = {}): {
  provider: IsolationProvider;
  acquired: string[];
  reaped: { force: boolean }[];
} {
  const acquired: string[] = [];
  const reaped: { force: boolean }[] = [];
  const provider: IsolationProvider = {
    tag: "fake-fs",
    async acquire({ taskId }): Promise<IsolationHandle> {
      acquired.push(taskId);
      return { cwd: `/fs/${taskId}`, meta: { branch: branchFor(taskId) } };
    },
    async reap(_ctx, { force }): Promise<IsolationReapResult> {
      reaped.push({ force });
      return opts.reapResult ?? { removed: true, dirty: false };
    },
  };
  return { provider, acquired, reaped };
}

// ===========================================================================
// UNIT (no docker).
// ===========================================================================

describe("@autosk/docker — unit (argv / naming)", () => {
  test("containerName is autosk-<slugFor(canonRoot)>-<task>, deterministic, reuses the worktree slug", () => {
    const root = mkTemp("dk-name-");
    const canon = realpathSync(root);
    const name = containerName(root, "ask-deadbe");
    expect(name).toBe(`autosk-${slugFor(canon)}-ask-deadbe`);
    expect(name).toContain(slugFor(canon)); // same slug the inner worktree uses
    expect(containerName(root, "ask-deadbe")).toBe(name); // deterministic
    expect(/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(name)).toBe(true); // docker-legal
  });

  test("containerName rejects empty root / task", () => {
    expect(() => containerName("", "ask-1")).toThrow(/project root is empty/);
    expect(() => containerName("/tmp/p", "")).toThrow(/task id is empty/);
  });

  test("dockerExecArgv rewrites to `docker exec -i -w <cwd> -e <env...> <name> <cmd...>`", () => {
    expect(
      dockerExecArgv("docker", "c1", ["pi", "--mode", "rpc"], { cwd: "/w", env: { A: "1", B: "2" } }),
    ).toEqual(["docker", "exec", "-i", "-w", "/w", "-e", "A=1", "-e", "B=2", "c1", "pi", "--mode", "rpc"]);
    // No env → no -e flags.
    expect(dockerExecArgv("podman", "c2", ["pwd"], { cwd: "/x" })).toEqual([
      "podman", "exec", "-i", "-w", "/x", "c2", "pwd",
    ]);
  });

  test("runArgsFor builds the create argv: 1:1 worktree mount, UDS mount + AUTOSK_SOCK, env/runArgs, keep-alive", () => {
    const args = runArgsFor(
      "c1",
      "/wt/path",
      {
        image: "img:tag",
        socketPath: "/run/daemon.sock",
        env: { X: "y" },
        runArgs: ["--network", "none"],
        autoskBin: "/host/autosk",
      },
      true,
    );
    expect(args).toEqual([
      "run", "-d", "--name", "c1",
      "-v", "/wt/path:/wt/path",
      "-v", "/run/daemon.sock:/run/daemon.sock", "-e", "AUTOSK_SOCK=/run/daemon.sock",
      "-v", "/host/autosk:/usr/local/bin/autosk:ro",
      "-e", "X=y",
      "--network", "none",
      "--entrypoint", "tail", "img:tag", "-f", "/dev/null",
    ]);
  });

  test("runArgsFor with mountSocket=false omits the socket mount + AUTOSK_SOCK", () => {
    const args = runArgsFor("c1", "/wt", { image: "img" }, false);
    expect(args).toEqual(["run", "-d", "--name", "c1", "-v", "/wt:/wt", "--entrypoint", "tail", "img", "-f", "/dev/null"]);
    expect(args).not.toContain("AUTOSK_SOCK=/run/daemon.sock");
  });

  test("resolveSocketPath: override → $AUTOSK_SOCK → <home>/.autosk/daemon.sock", () => {
    const saved = process.env.AUTOSK_SOCK;
    try {
      expect(resolveSocketPath("/explicit.sock", "/home/u")).toBe("/explicit.sock");
      delete process.env.AUTOSK_SOCK;
      expect(resolveSocketPath(undefined, "/home/u")).toBe("/home/u/.autosk/daemon.sock");
      process.env.AUTOSK_SOCK = "/env.sock";
      expect(resolveSocketPath(undefined, "/home/u")).toBe("/env.sock");
    } finally {
      if (saved === undefined) delete process.env.AUTOSK_SOCK;
      else process.env.AUTOSK_SOCK = saved;
    }
  });

  test("dockerIsolation requires an image", () => {
    expect(() => dockerIsolation({ image: "" })).toThrow(/image.*required/i);
    // @ts-expect-error — exercising the runtime guard with a missing image.
    expect(() => dockerIsolation({})).toThrow(/image.*required/i);
    expect(dockerIsolation({ image: "img" }).tag).toBe("docker");
  });
});

// ===========================================================================
// LIFECYCLE (hermetic: fake docker + fake inner).
// ===========================================================================

describe("@autosk/docker — lifecycle (fake docker CLI)", () => {
  test("acquire on an ABSENT container: inspect → run (creates it), handle carries the container name", async () => {
    const fd = withFakeDocker();
    const inner = fakeInner();
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, inner: inner.provider, mountSocket: false });
    const handle = await prov.acquire({ projectRoot: "/proj", taskId: "ask-abc" });

    const name = containerName("/proj", "ask-abc");
    expect(handle.cwd).toBe("/fs/ask-abc"); // delegated to the inner FS provider
    expect(handle.meta?.container).toBe(name);
    expect(handle.meta?.branch).toBe(branchFor("ask-abc")); // inner meta preserved
    expect(typeof handle.exec).toBe("function"); // the seam is present
    expect(typeof handle.spawn).toBe("function");
    expect(inner.acquired).toEqual(["ask-abc"]);

    const log = fd.lines();
    expect(log.some((l) => l.startsWith(`inspect -f {{.State.Running}} ${name}`))).toBe(true);
    expect(log.some((l) => l.startsWith(`run -d --name ${name}`))).toBe(true);
    expect(log.some((l) => l.includes("start"))).toBe(false); // absent → run, not start
  });

  test("acquire on a STOPPED container starts it (no run)", async () => {
    const fd = withFakeDocker();
    const name = containerName("/proj", "ask-stop");
    writeFileSync(join(fd.stateDir, name), "false"); // seed: exists but stopped
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, inner: fakeInner().provider, mountSocket: false });
    await prov.acquire({ projectRoot: "/proj", taskId: "ask-stop" });

    const log = fd.lines();
    expect(log.some((l) => l.startsWith(`start ${name}`))).toBe(true);
    expect(log.some((l) => l.startsWith("run -d"))).toBe(false);
  });

  test("acquire on a RUNNING container reuses it (no run, no start)", async () => {
    const fd = withFakeDocker();
    const name = containerName("/proj", "ask-run");
    writeFileSync(join(fd.stateDir, name), "true"); // seed: running
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, inner: fakeInner().provider, mountSocket: false });
    await prov.acquire({ projectRoot: "/proj", taskId: "ask-run" });

    const log = fd.lines();
    expect(log.some((l) => l.startsWith("run -d"))).toBe(false);
    expect(log.some((l) => l.startsWith(`start ${name}`))).toBe(false);
    expect(log.some((l) => l.startsWith(`inspect`))).toBe(true);
  });

  test("release stops the container (kept for resume)", async () => {
    const fd = withFakeDocker();
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, inner: fakeInner().provider, mountSocket: false });
    const handle = await prov.acquire({ projectRoot: "/proj", taskId: "ask-rel" });
    const name = containerName("/proj", "ask-rel");
    expect(readFileSync(join(fd.stateDir, name), "utf8")).toBe("true"); // running after acquire

    await prov.release!(handle);
    expect(readFileSync(join(fd.stateDir, name), "utf8")).toBe("false"); // stopped, file kept
    expect(fd.lines().some((l) => l.startsWith(`stop ${name}`))).toBe(true);
  });

  test("reap: clean inner → rm -f; container file removed", async () => {
    const fd = withFakeDocker();
    const inner = fakeInner({ reapResult: { removed: true, dirty: false } });
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, inner: inner.provider, mountSocket: false });
    await prov.acquire({ projectRoot: "/proj", taskId: "ask-reap" });
    const name = containerName("/proj", "ask-reap");

    const res = await prov.reap!({ projectRoot: "/proj", taskId: "ask-reap" }, { force: true });
    expect(res).toEqual({ removed: true, dirty: false });
    expect(inner.reaped).toEqual([{ force: true }]); // dirty-gate delegated to the inner
    expect(existsSync(join(fd.stateDir, name))).toBe(false); // container removed
    expect(fd.lines().some((l) => l.startsWith(`rm -f ${name}`))).toBe(true);
  });

  test("reap: dirty inner without force is REFUSED — container left in place (no rm)", async () => {
    const fd = withFakeDocker();
    const inner = fakeInner({ reapResult: { removed: false, dirty: true, detail: "1 uncommitted file(s)" } });
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, inner: inner.provider, mountSocket: false });
    await prov.acquire({ projectRoot: "/proj", taskId: "ask-dirty" });
    const name = containerName("/proj", "ask-dirty");

    const res = await prov.reap!({ projectRoot: "/proj", taskId: "ask-dirty" }, { force: false });
    expect(res).toEqual({ removed: false, dirty: true, detail: "1 uncommitted file(s)" });
    expect(existsSync(join(fd.stateDir, name))).toBe(true); // LEFT in place
    expect(fd.lines().some((l) => l.startsWith("rm"))).toBe(false); // no rm issued
  });

  test("acquire FAILS clearly when the container does not stay running (bad keep-alive)", async () => {
    const fd = withFakeDocker({ ephemeral: true });
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, inner: fakeInner().provider, mountSocket: false });
    // run exits 0 but the container is not running afterwards → a descriptive throw,
    // not a misleading "container is not running" surfacing later on the first exec.
    await expect(prov.acquire({ projectRoot: "/proj", taskId: "ask-dead" })).rejects.toThrow(
      /container is not running|keep-alive/i,
    );
  });

  test("reap is idempotent: a second reap on an already-gone container is a no-op result", async () => {
    const fd = withFakeDocker();
    const inner = fakeInner({ reapResult: { removed: false, dirty: false } });
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, inner: inner.provider, mountSocket: false });
    const name = containerName("/proj", "ask-gone");
    // No acquire — reap by identity must work with no live handle / no container.
    const res = await prov.reap!({ projectRoot: "/proj", taskId: "ask-gone" }, { force: true });
    expect(res.removed).toBe(false);
    expect(existsSync(join(fd.stateDir, name))).toBe(false);
    expect(fd.lines().some((l) => l.startsWith(`rm -f ${name}`))).toBe(true); // best-effort rm, tolerated
  });
});

// ===========================================================================
// COMPOSITION (gated on git: fake docker + the REAL worktree inner).
// ===========================================================================

describe("@autosk/docker — composition with worktreeIsolation (real git, fake docker)", () => {
  test("branch created on acquire; worktree gone + branch PRESERVED on reap", async () => {
    if (!gitOk) return;
    const fd = withFakeDocker();
    const home = mkTemp("dk-home-");
    const root = makeRepo();
    const taskId = "ask-compose";
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, home, mountSocket: false });

    const handle = await prov.acquire({ projectRoot: root, taskId });
    // FS lifecycle delegated to worktreeIsolation: real worktree dir + branch.
    expect(handle.cwd).toBe(pathFor(root, taskId, home));
    expect(existsSync(handle.cwd)).toBe(true);
    expect(handle.meta?.branch).toBe(branchFor(taskId));
    expect(branchListed(root, branchFor(taskId))).toBe(true);
    expect(handle.meta?.container).toBe(containerName(root, taskId));

    const res = await prov.reap!({ projectRoot: root, taskId }, { force: true });
    expect(res.removed).toBe(true);
    expect(existsSync(handle.cwd)).toBe(false); // worktree removed
    expect(branchListed(root, branchFor(taskId))).toBe(true); // branch PRESERVED
  });

  test("dirty worktree is refused without force (composition), removed with force", async () => {
    if (!gitOk) return;
    const fd = withFakeDocker();
    const home = mkTemp("dk-home-");
    const root = makeRepo();
    const taskId = "ask-cdirty";
    const prov = dockerIsolation({ image: "img", dockerBin: fd.dockerBin, home, mountSocket: false });

    const handle = await prov.acquire({ projectRoot: root, taskId });
    writeFileSync(join(handle.cwd, "scratch.txt"), "uncommitted"); // untracked → dirty

    const refused = await prov.reap!({ projectRoot: root, taskId }, { force: false });
    expect(refused.removed).toBe(false);
    expect(refused.dirty).toBe(true);
    expect(existsSync(handle.cwd)).toBe(true); // left in place

    const forced = await prov.reap!({ projectRoot: root, taskId }, { force: true });
    expect(forced.removed).toBe(true);
    expect(existsSync(handle.cwd)).toBe(false);
    expect(branchListed(root, branchFor(taskId))).toBe(true); // branch PRESERVED
  });
});

// ===========================================================================
// INTEGRATION (gated on a real docker + image).
// ===========================================================================

describe("@autosk/docker — integration (real docker)", () => {
  test("exec/spawn run INSIDE the container (pwd == worktree path; -e var visible); release stops; reap removes", async () => {
    if (!gitOk || !dockerOk || !imageOk) return;
    const home = mkTemp("dk-ihome-");
    const root = makeRepo();
    const taskId = "ask-int01";
    // The test image (busybox) has no `pi`/`autosk`; we only exercise the seam
    // with shell builtins, so skip the socket mount (busybox has no autosk).
    const prov = dockerIsolation({ image: TEST_IMAGE, home, mountSocket: false });
    const name = containerName(root, taskId);
    const ctrl = new AbortController();

    try {
      const handle = await prov.acquire({ projectRoot: root, taskId });
      expect(handle.exec && handle.spawn).toBeTruthy();

      // `pwd` runs INSIDE the container at the 1:1 bind-mounted worktree path.
      const pwd = await handle.exec!(["pwd"], { cwd: handle.cwd, signal: ctrl.signal });
      expect(pwd.code).toBe(0);
      expect(pwd.stdout.trim()).toBe(handle.cwd);

      // A `-e` var passed through the seam is visible inside the container.
      const envOut = await handle.exec!(["sh", "-c", "printf %s \"$FOO\""], {
        cwd: handle.cwd,
        env: { FOO: "bar123" },
        signal: ctrl.signal,
      });
      expect(envOut.stdout).toBe("bar123");

      // `ExecOptions.input` is piped through `docker exec -i` to the in-container
      // process (mirrors the host path; e.g. a `git apply` patch on stdin).
      const piped = await handle.exec!(["sh", "-c", "cat"], {
        cwd: handle.cwd,
        input: "ping",
        signal: ctrl.signal,
      });
      expect(piped.stdout).toBe("ping");

      // `ExecOptions.timeoutMs` kills the host `docker exec` client and resolves a
      // non-zero ExecResult quickly (mirrors the host path's timeout guard).
      const t0 = Date.now();
      const timedOut = await handle.exec!(["sh", "-c", "sleep 10"], {
        cwd: handle.cwd,
        timeoutMs: 100,
        signal: ctrl.signal,
      });
      expect(timedOut.code).not.toBe(0);
      expect(Date.now() - t0).toBeLessThan(9000); // returned well before the 10s sleep

      // spawn streams line-buffered stdout from inside the container.
      const lines: string[] = [];
      const child = handle.spawn!(["sh", "-c", "echo line1; echo line2"], {
        cwd: handle.cwd,
        signal: ctrl.signal,
      });
      child.onStdout((l) => lines.push(l));
      await child.exited;
      expect(lines).toEqual(["line1", "line2"]);

      // release stops the container but keeps it (resumable).
      await prov.release!(handle);
      const stopped = Bun.spawnSync(["docker", "inspect", "-f", "{{.State.Running}}", name]);
      expect(stopped.stdout.toString().trim()).toBe("false");

      // acquire again resumes the stopped container (start, not run).
      const handle2 = await prov.acquire({ projectRoot: root, taskId });
      const running = Bun.spawnSync(["docker", "inspect", "-f", "{{.State.Running}}", name]);
      expect(running.stdout.toString().trim()).toBe("true");
      expect(handle2.cwd).toBe(handle.cwd);
    } finally {
      // reap removes the container + worktree, preserving the branch.
      const res = await prov.reap!({ projectRoot: root, taskId }, { force: true });
      expect(res.removed).toBe(true);
      expect(existsSync(pathFor(root, taskId, home))).toBe(false);
      expect(branchListed(root, branchFor(taskId))).toBe(true);
      const gone = Bun.spawnSync(["docker", "inspect", name]);
      expect(gone.exitCode).not.toBe(0); // container removed
      ctrl.abort();
    }
  }, 120000);
});
