/**
 * Engine test scaffolding: build in-process projects (file store + an extension
 * registry populated directly, bypassing the loader) and an {@link Engine} over
 * them, with no RPC server. This is the "drivable purely from tests" surface the
 * P4 acceptance criteria require.
 */

import type { AgentDefinition, StepTarget, WorkflowDefinition } from "@autosk/sdk";

import {
  CapturingLogger,
  Engine,
  ExtensionRegistry,
  Store,
  type Clock,
  type EngineProject,
} from "../src/index.ts";
import { tempDir } from "./helpers.ts";

/** A project built for a test: its store, registry, canonical root, and cleanup. */
export interface TestProject {
  project: EngineProject;
  store: Store;
  registry: ExtensionRegistry;
  root: string;
  cleanup: () => void;
}

/** Builds a fresh on-disk project with the given workflows registered (agents are inline steps). */
export async function makeProject(opts: {
  workflows?: WorkflowDefinition[];
  clock?: Clock;
} = {}): Promise<TestProject> {
  const dir = tempDir();
  const store = new Store(dir.path, { watch: false, clock: opts.clock });
  await store.open();
  const registry = new ExtensionRegistry();
  for (const wf of opts.workflows ?? []) registry.addWorkflow("test", wf);
  const project: EngineProject = { root: store.root, store, registry };
  return { project, store, registry, root: store.root, cleanup: () => dir.cleanup() };
}

/** An engine with a silent capturing logger (keeps suite output clean). */
export function makeEngine(opts: { workers?: number; rescanMs?: number; clock?: Clock } = {}): {
  engine: Engine;
  logger: CapturingLogger;
} {
  const logger = new CapturingLogger();
  // Default the safety rescan OFF in tests so a stray timer can never make a
  // suite flaky; tests that exercise it opt in via an explicit `rescanMs`.
  const engine = new Engine({
    workers: opts.workers,
    rescanMs: opts.rescanMs ?? 0,
    clock: opts.clock,
    logger,
  });
  return { engine, logger };
}

/** A trivial inline agent that transits to a fixed target on `onRun`. */
export function transitAgent(to: StepTarget): AgentDefinition {
  return { onRun: async (ctx) => void (await ctx.transit(to)) };
}

/** A one-step workflow `do → <agent>` with the agent inlined as the `do` step. */
export function oneStep(
  name: string,
  agent: AgentDefinition,
  isolation?: WorkflowDefinition["isolation"],
): WorkflowDefinition {
  const wf: WorkflowDefinition = { name, firstStep: "do", steps: { do: agent } };
  if (isolation) wf.isolation = isolation;
  return wf;
}

/** A controllable barrier: a promise plus its resolver, for gating agents. */
export function gate(): { wait: Promise<void>; open: () => void } {
  let open!: () => void;
  const wait = new Promise<void>((resolve) => {
    open = resolve;
  });
  return { wait, open };
}
