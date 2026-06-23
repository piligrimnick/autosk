/**
 * `@autosk/sandbox` — the userspace isolation library (plan §4).
 *
 * Isolation left the SDK/engine with the abolition of the `IsolationProvider`
 * abstraction; agents own it via this package. It exposes the STRUCTURAL
 * {@link Sandbox} shape (documented, not a nominal contract agents must import),
 * the `worktreeSandbox()` / `dockerSandbox()` factories, and the
 * `sandboxCleanupStep()` workflow-step helper. It absorbs the deterministic
 * slug / branch / container-name derivations from the retired `@autosk/worktree`
 * and `@autosk/docker` packages (byte-identical, so already-allocated
 * worktrees/branches/containers still resolve).
 */

export type { Sandbox, SandboxCleanupResult, SandboxWrapOptions, TaskIdentity } from "./types.ts";
export { worktreeSandbox, branchFor, pathFor, slugFor, canonRoot, type WorktreeSandboxOptions } from "./worktree.ts";
export {
  dockerSandbox,
  containerName,
  runArgv,
  resolveSocketPath,
  type DockerSandboxOptions,
  type DockerMount,
} from "./docker.ts";
export { sandboxCleanupStep, type SandboxCleanupStepOptions } from "./cleanup.ts";
