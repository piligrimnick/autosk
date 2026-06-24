/**
 * FeatureGrid source of truth (plan §6.6). Reorder freely — the grid renders
 * straight from this array. Lead with the genuinely novel selling points:
 * workflows-as-code and multi-agent handoffs come first.
 *
 * `icon` keys map to the inline SVG set in components/ui/Icon.astro.
 */
export type Feature = {
  icon: string;
  title: string;
  body: string;
  /** Optional accent for the icon chip; defaults to the brand blue. */
  accent?: "accent" | "accent-2" | "human" | "step" | "agent";
};

export const features: Feature[] = [
  {
    icon: "code",
    accent: "accent",
    title: "Workflows as code, not config",
    body: "A workflow is a TypeScript graph of steps, each owned by an agent, versioned in your repo. No database, no brittle JSON editor.",
  },
  {
    icon: "handoff",
    accent: "agent",
    title: "Multi-agent handoffs",
    body: "Agents pass work through comments; every prior comment is injected at the top of the next step's prompt, so context carries between roles.",
  },
  {
    icon: "shield",
    accent: "human",
    title: "Guardrails that stop loops",
    body: "step_visits caps how many times a task can bounce through a step. After N failed reviews it parks for a human — not forever.",
  },
  {
    icon: "lifebuoy",
    accent: "accent-2",
    title: "Never lose work",
    body: "Any run that can't transition cleanly is parked to human. Crash recovery seals interrupted sessions so work always lands somewhere a person can pick it up.",
  },
  {
    icon: "boxes",
    accent: "step",
    title: "Agent-owned sandboxes",
    body: "Run each task in its own git worktree (autosk/<task-id>) or a per-task Docker container, so parallel agents never clobber each other's files.",
  },
  {
    icon: "database",
    accent: "accent-2",
    title: "No database — just files",
    body: "Tasks, comments, and transcripts are plain files under .autosk/. Hand-editable, git-diffable, no migrator, no engine.",
  },
  {
    icon: "screens",
    accent: "accent",
    title: "Three front ends, one source of truth",
    body: "A lazygit-style TUI, a native Tauri desktop GUI (+ iPhone/iPad), and the CLI — all pure JSON-RPC clients of one daemon, all streaming the same live transcript.",
  },
  {
    icon: "plug",
    accent: "agent",
    title: "Bring your own agent",
    body: "Ships an agent that drives pi and one that drives Claude Code headless. Swap them inline in a workflow, or write your own.",
  },
  {
    icon: "puzzle",
    accent: "step",
    title: "Extensions, no build step",
    body: "Drop a .ts file in .autosk/extensions/ or run autosk ext add npm:@scope/pkg. A broken extension shows up in diagnostics instead of crashing the daemon.",
  },
];
