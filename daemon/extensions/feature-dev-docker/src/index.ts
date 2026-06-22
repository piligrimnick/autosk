/**
 * `@autosk/feature-dev-docker` — the Docker variant of `@autosk/feature-dev`.
 *
 * It registers the `feature-dev-docker` workflow: the shipped
 * `featureDevWorkflow()` graph VERBATIM (dev → review → docs → validator →
 * accept → cleanup → done, with the dev visit-cap and the review→dev /
 * validator→dev bounce-backs), only swapping the default `worktreeSandbox()` for
 * a credential- and git-aware `dockerSandbox` so every agent step runs inside a
 * per-task `docker run -i --rm` container (`ghcr.io/wierdbytes/pi-runtime`)
 * authenticated with the OPERATOR's host pi credentials.
 *
 * pi runs THIN: with `dockerSandbox` (`thin === true`) the agent mints a
 * per-session host HTTP MCP server and injects the ack-only `autosk_transit`
 * tool, while the transport-aware `@autosk/pi-tools` (loaded from the mounted
 * `~/.pi`) routes `autosk_task`/`autosk_comment` through that server over
 * `host.docker.internal` (via `AUTOSK_MCP_URL`) — so the image needs neither
 * `autosk` nor a mounted daemon socket.
 *
 *   1. Auth — pi keeps its provider tokens in `~/.pi/agent/auth.json` (a portable
 *      file, NOT a Keychain), so the host `~/.pi` is simply bind-mounted at the
 *      container's `/home/agent/.pi` (read-write — pi writes sessions/tmp and may
 *      refresh tokens). No export step is needed.
 *   2. Git — `dockerSandbox` only bind-mounts the per-task git WORKTREE
 *      (`-v cwd:cwd`). A worktree's `.git` is a pointer file into
 *      `<projectRoot>/.git/worktrees/<id>`, so without the project `.git` mounted
 *      at its identical path, in-container `git` (and `go`/`make` shelling out to
 *      it) cannot resolve the repo. `dockerSandbox`'s static `mounts` cannot
 *      express the per-task project root, so we layer that mount in at `wrap()`.
 *
 * Discovery: NOT bootstrapped. Install it explicitly — `autosk ext add
 * npm:@autosk/feature-dev-docker` (or from a local checkout). Build/pull the
 * `pi-runtime` image first (see `@autosk/pi-agent`'s `docker/`).
 *
 * Env knobs (all optional):
 *   AUTOSK_PI_DOCKER_IMAGE  image to run   (default ghcr.io/wierdbytes/pi-runtime:latest)
 *   AUTOSK_PI_DIR           host pi config (default ~/.pi)
 */

import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

import type { AutoskAPI } from "@autosk/sdk";
import {
  canonRoot,
  dockerSandbox,
  type DockerMount,
  type Sandbox,
  type SandboxWrapOptions,
} from "@autosk/sandbox";
import { featureDevWorkflow } from "@autosk/feature-dev";

const WORKFLOW_NAME = "feature-dev-docker";
const CONTAINER_HOME = "/home/agent";

/** The operator image (a thin `pi` + toolchain runtime). */
export function defaultDockerImage(): string {
  const e = process.env.AUTOSK_PI_DOCKER_IMAGE;
  return e && e.trim() !== "" ? e : "ghcr.io/wierdbytes/pi-runtime:latest";
}

/** Host pi config dir (auth.json, models.json, settings.json, extensions, skills). */
function piDir(): string {
  const e = process.env.AUTOSK_PI_DIR;
  return e && e.trim() !== "" ? e : join(homedir(), ".pi");
}

/** Static container mounts: host `~/.pi` (rw) carries pi's provider auth + model config. */
function buildMounts(): DockerMount[] {
  const dir = piDir();
  if (!existsSync(dir)) {
    console.warn(
      `[${WORKFLOW_NAME}] no pi config dir at ${dir} — the in-container pi will be ` +
        `UNAUTHENTICATED. Log in to pi on the host (it writes ~/.pi/agent/auth.json).`,
    );
    return [];
  }
  return [{ hostPath: dir, sandboxPath: `${CONTAINER_HOME}/.pi` }];
}

/**
 * A {@link dockerSandbox} that ALSO bind-mounts the per-task project's `.git` at
 * its identical host path, so an in-container git worktree resolves its commondir
 * (`<projectRoot>/.git/worktrees/<id>`). The static `mounts` option cannot name
 * the per-task project root, so the mount is layered in at `wrap()` time, just
 * before the `<image>` token in the `docker run …` argv.
 */
export function featureDevDockerSandbox(): Sandbox {
  const image = defaultDockerImage();
  const base = dockerSandbox({ image, home: CONTAINER_HOME, mounts: buildMounts() });
  return {
    ...base,
    wrap(cmd: string[], o: SandboxWrapOptions): string[] {
      const argv = base.wrap(cmd, o);
      const gitDir = join(canonRoot(o.id.projectRoot), ".git");
      if (!existsSync(gitDir)) return argv;
      const at = argv.indexOf(image);
      if (at < 0) return argv;
      return [...argv.slice(0, at), "-v", `${gitDir}:${gitDir}`, ...argv.slice(at)];
    },
  };
}

/** The extension factory: registers the `feature-dev-docker` workflow. */
export default function featureDevDockerExtension(autosk: AutoskAPI): void {
  const base = featureDevWorkflow({ sandbox: featureDevDockerSandbox() });
  autosk.registerWorkflow({
    ...base,
    name: WORKFLOW_NAME,
    description:
      "Full feature-development cycle (pi, Docker): dev → review → docs → validator → accept (human) → " +
      "cleanup → done, each agent running inside a per-task `docker run -i --rm` container " +
      "(ghcr.io/wierdbytes/pi-runtime) authenticated with the host ~/.pi, reaching the host MCP over " +
      "host.docker.internal.",
  });
}
