/**
 * `@autosk/merge-to-current` — a single-step workflow that merges the
 * autosk-managed branch `autosk/<task-id>` of a task INTO the branch the
 * operator currently has checked out (the CURRENT branch — whatever `HEAD`
 * points at), running NON-ISOLATED in the project's working tree.
 *
 * Ported from the v1 `merge-to-main` export (`.autosk/merge-to-main.workflow.json`),
 * with the destination changed from the mainline branch (`main`/`master`) to the
 * project's CURRENT branch: there is no `main`/`master` detection and no branch
 * switch — the merge lands on the branch `HEAD` already points at, and that is
 * also the rollback target. The single `merge` step is a `@autosk/pi-agent`
 * driving git directly; it fast-forwards when the current branch has not moved
 * since the fork point, otherwise it creates a `--no-ff` merge commit. It either
 * lands cleanly (`done`) or fully rolls back and parks for a human (`human`).
 *
 * Discovery: published to npm and declared as an extension via
 * `package.json#autosk.extensions`. Enroll a task with
 * `autosk enroll <task-id> --workflow merge-to-current`.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { type AutoskAPI, type WorkflowDefinition } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";

/** Reads the merge prompt (shipped under `prompts/`) to seed the agent's first message. */
function readPrompt(name: string): string {
  return readFileSync(fileURLToPath(new URL(`../prompts/${name}.md`, import.meta.url)), "utf8");
}

/**
 * The `merge-to-current` workflow definition. A single non-isolated `merge` step
 * (a `@autosk/pi-agent`) that integrates `autosk/<task-id>` into the current
 * branch and transitions to `done` (clean landing) or `human` (rolled back).
 *
 * No `isolation` field: the workflow runs in the project root (`tag: "none"`),
 * because it operates directly on the operator's working tree.
 */
export function mergeToCurrentWorkflow(): WorkflowDefinition {
  return {
    name: "merge-to-current",
    description:
      "Merge the autosk-managed branch `autosk/<task-id>` of the current task INTO the project's " +
      "CURRENT branch (whatever HEAD points at). Non-isolated: runs in the project's working tree. " +
      "Auto-commits any pending edits found in the task's worktree to the task branch before merging; " +
      "refuses if the project root has a dirty working tree or detached HEAD. Fast-forwards when the " +
      "current branch has not moved since the fork, otherwise creates a --no-ff merge commit. On " +
      "rollback the current branch is reset to where it started; on success it lands the merge there.",
    firstStep: "merge",
    steps: {
      // The step key IS the agent name; registering the workflow registers this
      // inline agent. `thinking: "high"` matches the v1 export's agent params.
      merge: piAgent({ thinking: "high", firstMessage: readPrompt("merge") }),
    },
  };
}

/** The extension factory: registering the workflow registers its inline `merge` agent. */
export default function mergeToCurrentExtension(autosk: AutoskAPI): void {
  autosk.registerWorkflow(mergeToCurrentWorkflow());
}
