/**
 * `@autosk/gh-review` — review a GitHub issue or pull request, read-only, in a
 * per-task docker container.
 *
 * A task is enrolled with a GitHub issue/PR URL in its title (or description):
 * `https://github.com/<owner>/<repo>/issues/9` or `…/pull/12`. The single
 * `review` step runs a pi agent inside `dockerSandbox({ image })` (a per-task
 * `docker run -i --rm` container, thin image — it reaches the daemon over the
 * per-session host HTTP MCP server, like `@autosk/feature-dev-docker`). The
 * agent reads the target with `gh` — authenticated with a READ-ONLY
 * fine-grained PAT, so every write is a 403 — reviews it, records the review
 * as a task comment, and transits to `accept`. Graph:
 *
 *   review ──▶ accept (human) ──▶ cleanup ──▶ done
 *
 * Read-only GitHub access: the operator provisions a fine-grained PAT
 * (Contents/Issues/Pull requests/Metadata: Read) in `~/.autosk/github/ro-token.json`
 * (`{ "token": "github_pat_…" }`). The extension reads the file and passes the
 * token to the container as the `GH_TOKEN` env — gh's NATIVE token mechanism:
 * no gh config file exists in the container, so gh never tries to rewrite one
 * (gh ≥2.40 rewrites `hosts.yml` on most invocations, which is incompatible
 * with a read-only mount), and `GH_NO_UPDATE_NOTIFIER=1` / `CHECKPOINT_DISABLE=1`
 * keep it from writing anything else. The token is visible in `docker inspect`
 * while the per-task container lives (minutes); read-only ACCESS itself is
 * enforced by the token's scopes server-side (a write → 403). `onTransit`
 * ({@link ghReviewGuard}) fail-fasts the enroll when the token file or a
 * parsable URL is missing.
 *
 * Env knobs (all optional):
 *   AUTOSK_GH_REVIEW_IMAGE  image to run   (default ghcr.io/wierdbytes/pi-runtime:latest)
 *   AUTOSK_GH_DIR           dir holding ro-token.json (default ~/.autosk/github)
 *   AUTOSK_PI_DIR           host pi config (default ~/.pi; auth + models, rw)
 *
 * Discovery: NOT bootstrapped — `autosk ext add npm:@autosk/gh-review` (build
 * or pull the pi-runtime image first; it ships `gh` — see @autosk/pi-agent's
 * docker/).
 */

import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  statusStep,
  type AutoskAPI,
  type StepTarget,
  type TransitContext,
  type WorkflowDefinition,
} from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import {
  canonRoot,
  dockerSandbox,
  sandboxCleanupStep,
  type DockerMount,
  type Sandbox,
  type SandboxWrapOptions,
} from "@autosk/sandbox";

import { parseGhTarget, type GhTarget } from "./parse.ts";

export { parseGhTarget, type GhTarget } from "./parse.ts";

export const WORKFLOW_NAME = "gh-review";
const CONTAINER_HOME = "/home/agent";

/** The operator image (a thin pi + toolchain runtime, with `gh`). */
export function defaultDockerImage(): string {
  const e = process.env.AUTOSK_GH_REVIEW_IMAGE;
  return e && e.trim() !== "" ? e : "ghcr.io/wierdbytes/pi-runtime:latest";
}

/** Host pi config dir (auth.json, models.json, settings.json, extensions, skills). */
function piDir(): string {
  const e = process.env.AUTOSK_PI_DIR;
  return e && e.trim() !== "" ? e : join(homedir(), ".pi");
}

/** Host dir holding the read-only gh token (`ro-token.json`). */
export function ghConfigDir(): string {
  const e = process.env.AUTOSK_GH_DIR;
  return e && e.trim() !== "" ? e : join(homedir(), ".autosk", "github");
}

/** The token file the operator must provision (see the README). */
export function ghTokenFile(): string {
  return join(ghConfigDir(), "ro-token.json");
}

/**
 * Reads the gh token from {@link ghTokenFile} (`{ "token": "…" }`). Returns
 * `undefined` when the file is absent; THROWS when it exists but is unusable
 * (invalid JSON / missing or empty `token`) — a loud failure beats a container
 * that can't authenticate.
 */
export function readGhToken(): string | undefined {
  const file = ghTokenFile();
  if (!existsSync(file)) return undefined;
  let parsed: unknown;
  try {
    parsed = JSON.parse(readFileSync(file, "utf8"));
  } catch (e) {
    throw new Error(
      `gh-review: ${file} is not valid JSON (${e instanceof Error ? e.message : String(e)}) — ` +
        `expected { "token": "github_pat_…" }`,
    );
  }
  const token =
    typeof parsed === "object" && parsed !== null ? (parsed as Record<string, unknown>).token : undefined;
  if (typeof token !== "string" || token.trim() === "") {
    throw new Error(`gh-review: ${file} must contain a non-empty "token" field — expected { "token": "github_pat_…" }`);
  }
  return token;
}

/** The enroll-gate check: is the gh token provisioned and usable? */
export function checkGhToken(): { ok: boolean; reason?: string } {
  try {
    if (readGhToken() !== undefined) return { ok: true };
    return {
      ok: false,
      reason:
        `gh-review: no gh token file at ${ghTokenFile()} — create it with a READ-ONLY fine-grained ` +
        `PAT (Contents/Issues/Pull requests/Metadata: Read): { "token": "github_pat_…" } (chmod 600). ` +
        `See the @autosk/gh-review README.`,
    };
  } catch (e) {
    return { ok: false, reason: e instanceof Error ? e.message : String(e) };
  }
}

/** Static container mounts: host `~/.pi` (rw — pi's provider auth + model config). */
function buildMounts(): DockerMount[] {
  const mounts: DockerMount[] = [];
  const pi = piDir();
  if (existsSync(pi)) {
    mounts.push({ hostPath: pi, sandboxPath: `${CONTAINER_HOME}/.pi` });
  } else {
    console.warn(
      `[${WORKFLOW_NAME}] no pi config dir at ${pi} — the in-container pi will be ` +
        `UNAUTHENTICATED. Log in to pi on the host (it writes ~/.pi/agent/auth.json).`,
    );
  }
  return mounts;
}

/**
 * The per-task docker {@link Sandbox} the `review` step runs in: the stock
 * `dockerSandbox` (thin pi-runtime image; the harness reaches the host MCP
 * server over `host.docker.internal`) plus the SAME project `.git` layering
 * `@autosk/feature-dev-docker` uses, so an in-container `git` resolves the
 * bind-mounted worktree (its `.git` is a pointer into `<projectRoot>/.git/…`).
 */
export function ghReviewSandbox(): Sandbox {
  const image = defaultDockerImage();
  const base = dockerSandbox({ image, home: CONTAINER_HOME, mounts: buildMounts() });
  return {
    ...base,
    wrap(cmd: string[], o: SandboxWrapOptions): string[] {
      const argv = base.wrap(cmd, o);
      const at = argv.indexOf(image);
      if (at < 0) return argv;
      const extra: string[] = [];
      // FRESH read of the token per run (rotation works without a daemon
      // restart). Absent → no GH_TOKEN (the enroll guard rejects anyway; an
      // in-container gh then fails with its own clear "auth required" error).
      // A malformed file THROWS (fail loud rather than run unauthenticated).
      const token = readGhToken();
      if (token !== undefined) {
        extra.push(
          "-e",
          `GH_TOKEN=${token}`,
          "-e",
          "GH_NO_UPDATE_NOTIFIER=1",
          "-e",
          "CHECKPOINT_DISABLE=1",
        );
      }
      // Layer the project .git at its identical path so an in-container git
      // resolves the bind-mounted worktree (same trick as feature-dev-docker).
      const gitDir = join(canonRoot(o.id.projectRoot), ".git");
      if (existsSync(gitDir)) extra.push("-v", `${gitDir}:${gitDir}`);
      return extra.length === 0 ? argv : [...argv.slice(0, at), ...extra, ...argv.slice(at)];
    },
  };
}

/** Reads a role prompt (shipped under `prompts/`) to seed the agent's first message. */
function readPrompt(role: string): string {
  return readFileSync(fileURLToPath(new URL(`../prompts/${role}.md`, import.meta.url)), "utf8");
}

/**
 * The `onTransit` guard. It validates the ENROLL (the first entry into
 * `review`): the task must carry a parsable GitHub issue/PR URL, and the
 * operator must have provisioned the gh config. Either failure comments the
 * reason and REJECTS the transition (the task stays `new`, ready to re-enroll
 * once the title / config is fixed). Every other transition flows untouched.
 */
export async function ghReviewGuard(ctx: TransitContext, to: StepTarget): Promise<void> {
  // Only the first entry into `review` (the enroll) is gated; a resumed task
  // (visits > 0) is past validation.
  if (!("step" in to) || to.step !== "review" || ctx.visits("review") !== 0) return;
  const gh = checkGhToken();
  if (!gh.ok) {
    await ctx.comment(gh.reason!);
    throw new Error(gh.reason!);
  }
  const task = await ctx.tasks.current();
  const target: GhTarget | undefined = parseGhTarget(task.title) ?? parseGhTarget(task.description);
  if (!target) {
    const msg =
      "gh-review: no GitHub issue/PR URL in the task title (or description) — expected a link like " +
      "https://github.com/<owner>/<repo>/issues/9 or https://github.com/<owner>/<repo>/pull/12. " +
      "Fix the title and re-enroll.";
    await ctx.comment(msg);
    throw new Error(msg);
  }
  await ctx.comment(
    `gh-review: reviewing ${target.kind === "pull" ? "PR" : "issue"} ${target.owner}/${target.repo}#${target.number}.`,
  );
}

/** Options for {@link ghReviewWorkflow} (tests inject a sandbox double). */
export interface GhReviewWorkflowOptions {
  /** Sandbox the review step runs in (and the cleanup step tears down). Default: `ghReviewSandbox()`. */
  sandbox?: Sandbox;
}

/**
 * The `gh-review` workflow: `review → accept (human) → cleanup → done`. A
 * factory (not a const) so tests can swap the sandbox without touching the
 * shipped default.
 */
export function ghReviewWorkflow(opts: GhReviewWorkflowOptions = {}): WorkflowDefinition {
  const sandbox = opts.sandbox ?? ghReviewSandbox();
  return {
    name: WORKFLOW_NAME,
    description:
      "Review a GitHub issue or PR (URL in the task title) with a pi agent in a per-task docker " +
      "container and a READ-ONLY gh: review → accept (human) → cleanup → done.",
    firstStep: "review",
    steps: {
      // Inline agent: the step key ("review") IS the agent name; registering the
      // workflow registers it. xhigh thinking — the review verdict is the point.
      review: piAgent({ sandbox, thinking: "xhigh", firstMessage: readPrompt("review") }),
      accept: statusStep("human"),
      // Teardown as a normal step: removes the worktree (branch preserved) and any
      // orphan container, then transits to `done` (the human resumes the parked
      // task into it — `autosk resume <id> --to cleanup`).
      cleanup: sandboxCleanupStep(sandbox),
    },
    onTransit: ghReviewGuard,
  };
}

/** The extension factory: registering the workflow registers its inline agents. */
export default function ghReviewExtension(autosk: AutoskAPI): void {
  // Surface a missing/unusable token file at load time (the enroll guard also
  // rejects per-task); never fails the extension load itself.
  const gh = checkGhToken();
  if (!gh.ok) console.warn(gh.reason);
  autosk.registerWorkflow(ghReviewWorkflow());
}
