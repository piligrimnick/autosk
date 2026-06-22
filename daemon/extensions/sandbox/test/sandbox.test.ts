/**
 * `@autosk/sandbox` tests (plan §10):
 *
 *  - worktreeSandbox: deterministic slug/branch/path derivations (byte-identical
 *    to the retired @autosk/worktree provider), workspace create/reuse, cleanup
 *    (clean / missing / dirty-refuse / force), branch preservation, the identity
 *    `wrap`, the loopback `endpointFor`, the no-op `stop`.
 *  - dockerSandbox: the `docker run -i --rm --add-host … -v ws:ws -w ws …` wrap,
 *    `host.docker.internal` endpointFor, byte-identical containerName, the
 *    image-required guard, and (gated on git, fake docker) cleanup composition.
 */

import { createHash } from "node:crypto";
import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import { chmodSync, existsSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, join } from "node:path";

import type { StepTarget } from "@autosk/sdk";

import {
  branchFor,
  containerName,
  dockerSandbox,
  pathFor,
  runArgv,
  sandboxCleanupStep,
  slugFor,
  worktreeSandbox,
  type Sandbox,
} from "../src/index.ts";

// ---------------------------------------------------------------------------
// temp / git probes.
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
beforeAll(() => {
  gitOk = Bun.spawnSync(["git", "--version"]).exitCode === 0;
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
function makeRepo(prefix: string): string {
  const root = mkTemp(prefix);
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
// a hermetic fake `docker` CLI (state on disk + a command log) for `docker run
// -i --rm` / `docker stop` / `docker rm -f`.
// ---------------------------------------------------------------------------

function withFakeDocker(): { dockerBin: string; stateDir: string; lines(): string[] } {
  const dir = mkTemp("sb-shim-");
  const stateDir = join(dir, "state");
  const logFile = join(dir, "log");
  Bun.spawnSync(["mkdir", "-p", stateDir]);
  writeFileSync(logFile, "");
  const shimPath = join(dir, "fake-docker");
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
  run) printf 'true' > "$STATE/$(san "$name")"; printf 'fakeid\\n'; exit 0 ;;
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

// ===========================================================================
// worktreeSandbox.
// ===========================================================================

describe("worktreeSandbox — derivations (byte-identical to v1)", () => {
  test("branchFor is autosk/<task>", () => {
    expect(branchFor("ask-deadbe")).toBe("autosk/ask-deadbe");
  });

  test("slugFor is basename-8hex(sha256(canonRoot))", () => {
    const canon = "/tmp/myproj";
    const want = `${basename(canon)}-${createHash("sha256").update(canon, "utf8").digest().subarray(0, 4).toString("hex")}`;
    expect(slugFor(canon)).toBe(want);
    expect(slugFor(canon).length).toBe("myproj-".length + 8);
  });

  test("pathFor lands under <home>/.autosk/worktrees/<slug>/<task>", () => {
    const root = mkTemp("sb-pf-");
    const canon = realpathSync(root);
    const p = pathFor(root, "ask-abc123", "/home/u");
    expect(p).toBe(join("/home/u", ".autosk", "worktrees", slugFor(canon), "ask-abc123"));
  });
});

describe("worktreeSandbox — workspace / cleanup (real git)", () => {
  test("workspace allocates the dir on a fresh branch; wrap is identity; endpointFor is loopback", async () => {
    if (!gitOk) return;
    const home = mkTemp("sb-home-");
    const root = makeRepo("sb-repo-");
    const sb = worktreeSandbox({ home });
    const id = { projectRoot: root, taskId: "ask-deadbe" };

    const ws = await sb.workspace(id);
    expect(ws.cwd).toBe(pathFor(root, "ask-deadbe", home));
    expect(existsSync(ws.cwd)).toBe(true);
    expect(branchListed(root, "autosk/ask-deadbe")).toBe(true);

    // Host workspace: wrap is identity, stop is a no-op, endpoint is loopback, and
    // it is NOT thin (so pi-agent keeps its pi-tools path; `autosk` is on PATH).
    expect(sb.wrap(["claude", "-p"], { cwd: ws.cwd, id })).toEqual(["claude", "-p"]);
    expect(sb.thin).toBeFalsy();
    expect(sb.endpointFor(54321)).toBe("http://127.0.0.1:54321");
    await sb.stop(id); // no throw

    // cleanup removes the dir, PRESERVES the branch.
    const r = await sb.cleanup(id, { force: true });
    expect(r.removed).toBe(true);
    expect(existsSync(ws.cwd)).toBe(false);
    expect(branchListed(root, "autosk/ask-deadbe")).toBe(true);
  });

  test("workspace reuses the same dir across steps", async () => {
    if (!gitOk) return;
    const home = mkTemp("sb-home-");
    const root = makeRepo("sb-repo-");
    const sb = worktreeSandbox({ home });
    const id = { projectRoot: root, taskId: "ask-keepit" };

    const w1 = await sb.workspace(id);
    const w2 = await sb.workspace(id);
    expect(w2.cwd).toBe(w1.cwd);
    expect(existsSync(w2.cwd)).toBe(true);
    await sb.cleanup(id, { force: true });
  });

  test("cleanup on a missing env is a no-op", async () => {
    if (!gitOk) return;
    const home = mkTemp("sb-home-");
    const root = makeRepo("sb-repo-");
    const sb = worktreeSandbox({ home });
    const r = await sb.cleanup({ projectRoot: root, taskId: "ask-never" }, { force: false });
    expect(r).toEqual({ removed: false, dirty: false });
  });

  test("cleanup without force REFUSES a dirty worktree; force removes it (branch kept)", async () => {
    if (!gitOk) return;
    const home = mkTemp("sb-home-");
    const root = makeRepo("sb-repo-");
    const sb = worktreeSandbox({ home });
    const id = { projectRoot: root, taskId: "ask-dirty1" };

    const ws = await sb.workspace(id);
    writeFileSync(join(ws.cwd, "scratch.txt"), "uncommitted"); // untracked → dirty

    const refused = await sb.cleanup(id, { force: false });
    expect(refused.removed).toBe(false);
    expect(refused.dirty).toBe(true);
    expect(refused.detail).toMatch(/uncommitted file/);
    expect(existsSync(ws.cwd)).toBe(true);

    const forced = await sb.cleanup(id, { force: true });
    expect(forced.removed).toBe(true);
    expect(existsSync(ws.cwd)).toBe(false);
    expect(branchListed(root, "autosk/ask-dirty1")).toBe(true);
  });
});

// ===========================================================================
// dockerSandbox.
// ===========================================================================

describe("dockerSandbox — unit (argv / naming)", () => {
  test("containerName is autosk-<slugFor(canonRoot)>-<task>, byte-identical to v1", () => {
    const root = mkTemp("sb-name-");
    const canon = realpathSync(root);
    const name = containerName(root, "ask-deadbe");
    expect(name).toBe(`autosk-${slugFor(canon)}-ask-deadbe`);
    expect(/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(name)).toBe(true);
  });

  test("dockerSandbox requires an image", () => {
    expect(() => dockerSandbox({ image: "" })).toThrow(/image.*required/i);
    // @ts-expect-error — exercising the runtime guard with a missing image.
    expect(() => dockerSandbox({})).toThrow(/image.*required/i);
  });

  test("wrap emits `docker run -i --rm --add-host … -v ws:ws -w ws -e … <image> <cmd…>`", () => {
    const sb = dockerSandbox({ image: "img:tag", user: "1000:2000", env: { A: "1" } });
    const id = { projectRoot: "/proj", taskId: "ask-w" };
    const argv = sb.wrap(["claude", "-p"], { cwd: "/wt/path", env: { B: "2" }, id });
    const name = containerName("/proj", "ask-w");
    expect(argv).toEqual([
      "docker", "run", "-i", "--rm", "--name", name,
      "--add-host=host.docker.internal:host-gateway",
      "-v", "/wt/path:/wt/path", "-w", "/wt/path",
      "--user", "1000:2000",
      "-e", "A=1", "-e", "B=2",
      "img:tag", "claude", "-p",
    ]);
  });

  test("runArgv with mountSocket bind-mounts the UDS + AUTOSK_SOCK; user='' omits --user", () => {
    const argv = runArgv(
      "docker",
      "c1",
      "/wt",
      ["pi", "--mode", "rpc"],
      undefined,
      { image: "img", user: "", mountSocket: true, socketPath: "/run/daemon.sock" },
    );
    expect(argv).toEqual([
      "docker", "run", "-i", "--rm", "--name", "c1",
      "--add-host=host.docker.internal:host-gateway",
      "-v", "/wt:/wt", "-w", "/wt",
      "-v", "/run/daemon.sock:/run/daemon.sock", "-e", "AUTOSK_SOCK=/run/daemon.sock",
      "img", "pi", "--mode", "rpc",
    ]);
  });

  test("endpointFor rewrites to host.docker.internal", () => {
    const sb = dockerSandbox({ image: "img" });
    expect(sb.endpointFor(45678)).toBe("http://host.docker.internal:45678");
  });

  test("thin is true by default (no autosk in the image) and false under the mountSocket escape hatch", () => {
    expect(dockerSandbox({ image: "img" }).thin).toBe(true);
    expect(dockerSandbox({ image: "img", mountSocket: true }).thin).toBe(false);
  });

  test("wrap bind-mounts roFiles identical-path read-only (so `pi -e <hostpath>` resolves in the container)", () => {
    const sb = dockerSandbox({ image: "img", user: "" });
    const id = { projectRoot: "/proj", taskId: "ask-ro" };
    const argv = sb.wrap(["pi", "--mode", "rpc"], {
      cwd: "/wt",
      id,
      roFiles: ["/host/ext/pi-transit-extension.ts"],
    });
    // The extension file is bind-mounted at its identical path, read-only.
    const i = argv.indexOf("/host/ext/pi-transit-extension.ts:/host/ext/pi-transit-extension.ts:ro");
    expect(i).toBeGreaterThan(0);
    expect(argv[i - 1]).toBe("-v");
    // ...and it comes before the image + command.
    expect(argv.indexOf("img")).toBeGreaterThan(i);
  });
});

describe("dockerSandbox — cleanup composition (real git, fake docker)", () => {
  test("workspace creates the worktree; cleanup removes it + rm -f the container, branch PRESERVED", async () => {
    if (!gitOk) return;
    const fd = withFakeDocker();
    const home = mkTemp("sb-dhome-");
    const root = makeRepo("sb-drepo-");
    const id = { projectRoot: root, taskId: "ask-compose" };
    const sb = dockerSandbox({ image: "img", dockerBin: fd.dockerBin, worktreeHome: home });

    const ws = await sb.workspace(id);
    expect(ws.cwd).toBe(pathFor(root, "ask-compose", home));
    expect(existsSync(ws.cwd)).toBe(true);
    expect(branchListed(root, branchFor("ask-compose"))).toBe(true);

    const r = await sb.cleanup(id, { force: true });
    expect(r.removed).toBe(true);
    expect(existsSync(ws.cwd)).toBe(false);
    expect(branchListed(root, branchFor("ask-compose"))).toBe(true);
    const name = containerName(root, "ask-compose");
    expect(fd.lines().some((l) => l.startsWith(`rm -f ${name}`))).toBe(true);
  });

  test("workspace force-removes a stale container (deterministic name is reused across steps)", async () => {
    if (!gitOk) return;
    const fd = withFakeDocker();
    const home = mkTemp("sb-dhome-");
    const root = makeRepo("sb-drepo-");
    const id = { projectRoot: root, taskId: "ask-preclean" };
    const sb = dockerSandbox({ image: "img", dockerBin: fd.dockerBin, worktreeHome: home });
    const name = containerName(root, "ask-preclean");
    await sb.workspace(id);
    // The next step's `docker run --name <det>` must never hit a leftover container.
    expect(fd.lines().some((l) => l.startsWith(`rm -f ${name}`))).toBe(true);
  });

  test("stop issues `docker stop <name>`", async () => {
    const fd = withFakeDocker();
    const sb = dockerSandbox({ image: "img", dockerBin: fd.dockerBin });
    const id = { projectRoot: "/proj", taskId: "ask-stop" };
    const name = containerName("/proj", "ask-stop");
    await sb.stop(id);
    expect(fd.lines().some((l) => l.startsWith(`stop ${name}`))).toBe(true);
  });

  test("cleanup leaves the container when the worktree is dirty (no rm)", async () => {
    if (!gitOk) return;
    const fd = withFakeDocker();
    const home = mkTemp("sb-dhome-");
    const root = makeRepo("sb-drepo-");
    const id = { projectRoot: root, taskId: "ask-cdirty" };
    const sb = dockerSandbox({ image: "img", dockerBin: fd.dockerBin, worktreeHome: home });

    const ws = await sb.workspace(id);
    writeFileSync(join(ws.cwd, "scratch.txt"), "uncommitted");
    // workspace() force-removes a stale container as a pre-clean, so baseline the
    // rm count and assert cleanup itself adds NO further rm when the tree is dirty.
    const rmsBefore = fd.lines().filter((l) => l.startsWith("rm")).length;

    const refused = await sb.cleanup(id, { force: false });
    expect(refused.removed).toBe(false);
    expect(refused.dirty).toBe(true);
    expect(existsSync(ws.cwd)).toBe(true);
    expect(fd.lines().filter((l) => l.startsWith("rm")).length).toBe(rmsBefore);
  });
});

// ===========================================================================
// sandboxCleanupStep.
// ===========================================================================

describe("sandboxCleanupStep", () => {
  /** A sandbox stub recording cleanup calls and returning a canned result. */
  function stubSandbox(result: { removed: boolean; dirty: boolean; detail?: string }): {
    sandbox: Sandbox;
    calls: { id: { projectRoot: string; taskId: string }; force: boolean }[];
  } {
    const calls: { id: { projectRoot: string; taskId: string }; force: boolean }[] = [];
    const sandbox: Sandbox = {
      workspace: async (id) => ({ cwd: id.projectRoot }),
      wrap: (cmd) => cmd,
      endpointFor: (port) => `http://127.0.0.1:${port}`,
      stop: async () => {},
      cleanup: async (id, { force }) => {
        calls.push({ id, force });
        return result;
      },
    };
    return { sandbox, calls };
  }

  /** A minimal task-mode ctx capturing comment + transit. */
  function fakeCtx(): { ctx: any; comments: string[]; transits: StepTarget[] } {
    const comments: string[] = [];
    const transits: StepTarget[] = [];
    const ctx = {
      projectRoot: "/proj",
      tasks: { currentId: "ask-clean" },
      comment: async (t: string) => void comments.push(t),
      transit: async (to: StepTarget) => void transits.push(to),
    };
    return { ctx, comments, transits };
  }

  test("removes the env, comments the outcome, and transits to done by default", async () => {
    const { sandbox, calls } = stubSandbox({ removed: true, dirty: false, detail: "3 uncommitted file(s)" });
    const step = sandboxCleanupStep(sandbox);
    const { ctx, comments, transits } = fakeCtx();
    await step.onRun(ctx);
    expect(calls).toEqual([{ id: { projectRoot: "/proj", taskId: "ask-clean" }, force: true }]);
    expect(comments[0]).toContain("cleaned up sandbox env");
    expect(comments[0]).toContain("3 uncommitted file(s)");
    expect(transits).toEqual([{ status: "done" }]);
  });

  test("is idempotent on a missing env (comments 'nothing to clean up') and honours a custom target + force", async () => {
    const { sandbox, calls } = stubSandbox({ removed: false, dirty: false });
    const step = sandboxCleanupStep(sandbox, { to: { status: "cancel" }, force: false });
    const { ctx, comments, transits } = fakeCtx();
    await step.onRun(ctx);
    expect(calls).toEqual([{ id: { projectRoot: "/proj", taskId: "ask-clean" }, force: false }]);
    expect(comments[0]).toBe("no sandbox env to clean up");
    expect(transits).toEqual([{ status: "cancel" }]);
  });
});
