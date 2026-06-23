/**
 * Prompt rendering for the claude-agent. Copied verbatim from `@autosk/pi-agent`
 * `src/prompt.ts` — the renderers are harness-agnostic (task + targets +
 * comments → text); copying (rather than importing) keeps the two agents
 * independently versioned/publishable.
 *
 * The transit channel is the `mcp__autosk__transit` tool, so the prompts
 * instruct the model to call the `transit` tool. The available targets are the
 * workflow's declared target set for the step (the conservative superset the
 * engine exposes via `ctx.workflows.current.targets`); the per-role guidance on
 * *which* target to pick lives in the role's `firstMessage`.
 */

import type { Comment, StepTarget, TaskView } from "@autosk/sdk";

/** Renders one {@link StepTarget} as the string an operator/model uses for `to`. */
export function targetLabel(t: StepTarget): string {
  return "step" in t ? t.step : t.status;
}

/** The deduplicated list of `to` labels valid for the current step. */
export function targetLabels(targets: StepTarget[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const t of targets) {
    const label = targetLabel(t);
    if (!seen.has(label)) {
      seen.add(label);
      out.push(label);
    }
  }
  return out;
}

export interface PromptContext {
  firstMessage: string;
  agentName: string;
  workflow: string;
  step: string;
  task: TaskView;
  targets: StepTarget[];
  comments: Comment[];
}

/** Builds the initial user prompt for a step run. */
export function renderInitialPrompt(ctx: PromptContext): string {
  const sb: string[] = [];
  const fm = ctx.firstMessage.replace(/\n+$/, "");
  if (fm !== "") sb.push(fm, "");

  sb.push(`You are agent "${ctx.agentName}" on step "${ctx.step}" of workflow "${ctx.workflow}".`);
  sb.push(`Task: ${ctx.task.id}`);
  if (ctx.task.title !== "") sb.push(`Title: ${ctx.task.title}`);
  if (ctx.task.description !== "") {
    sb.push("", "Description:", ctx.task.description);
  }

  sb.push("", "Available transitions (pick exactly one before you stop):");
  for (const label of targetLabels(ctx.targets)) sb.push(`  - ${label}`);

  sb.push(
    "",
    `When you have decided, call the \`transit\` tool exactly once with \`to\` set to one of: ${targetLabels(ctx.targets).join(", ")}.`,
    "Do not stop before calling the `transit` tool exactly once.",
  );

  if (ctx.comments.length > 0) {
    sb.push("", "Comments (oldest first):");
    for (const c of ctx.comments) sb.push(`  [${c.author}] ${c.text}`);
  } else {
    sb.push("", "No comments on this task yet.");
  }
  return sb.join("\n") + "\n";
}

/** The kickback message when the model ends a turn without calling the tool. */
export function kickbackMessage(taskId: string, targets: StepTarget[], attempt: number, max: number): string {
  const sb: string[] = [];
  sb.push(`You stopped without calling the \`transit\` tool on task ${taskId}.`);
  sb.push("Before you stop you MUST call the `transit` tool exactly once with `to` set to one of:");
  for (const label of targetLabels(targets)) sb.push(`  - transit { "to": "${label}" }`);
  sb.push(`This is correction attempt ${attempt} of ${max}. If you ignore it the run will be marked failed.`);
  return sb.join("\n") + "\n";
}

/** The corrective message when a chosen transition was rejected by `onTransit`. */
export function rejectionMessage(
  rejected: StepTarget,
  error: string,
  targets: StepTarget[],
  attempt: number,
  max: number,
): string {
  const sb: string[] = [];
  sb.push(`Your transition to "${targetLabel(rejected)}" was rejected: ${error}`);
  sb.push("Pick a different target and call the `transit` tool again with `to` set to one of:");
  for (const label of targetLabels(targets)) sb.push(`  - transit { "to": "${label}" }`);
  sb.push(`This is correction attempt ${attempt} of ${max}. If you ignore it the run will be marked failed.`);
  return sb.join("\n") + "\n";
}
