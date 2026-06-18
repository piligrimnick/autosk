# @autosk/claude-agent

Drive [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude -p`
headless stream-json mode) as an autoskd v2 **agent**. `claudeAgent({...})`
returns an [`AgentDefinition`](../../sdk/src/agent.ts) the engine can run for a
workflow step — the structural twin of [`@autosk/pi-agent`](../pi-agent/README.md),
with Claude Code as the harness instead of `pi --mode rpc` (design
`docs/plans/20260618-Claude-Code-Agent.md`).

## Usage

```ts
import { claudeAgent } from "@autosk/claude-agent";
import { worktreeIsolation } from "@autosk/worktree";

export default function (autosk) {
  autosk.registerWorkflow({
    name: "my-flow",
    firstStep: "dev",
    steps: {
      // The step key IS the agent name; registering the workflow registers
      // its inline agents.
      dev: claudeAgent({
        model: "sonnet",
        firstMessageFile: new URL("./prompts/dev.md", import.meta.url).pathname,
      }),
      review: claudeAgent({ firstMessageFile: ".../review.md" }),
    },
    isolation: worktreeIsolation(),
  });
}
```

A `claudeAgent({...})` is an inline **step value**: the step key is the agent
name (there is no `name` option — the driver takes its display name from
`ctx.workflows.current.step`), so registering the workflow registers its agents.
It is **not** bootstrapped by the reference `feature-dev` workflow — it is an
opt-in alternative harness you wire into your own workflow (or replace
`feature-dev`'s `piAgent({...})` roles with).

### Requirements

- **`claude`** (the Claude Code CLI) on `PATH`, or pointed at by
  `$AUTOSK_CLAUDE_BIN` / the `claudeBin` option. The headless run is unattended,
  so Claude Code must be already authenticated.
- **`autosk`** (the CLI) on `PATH`, or pointed at by `$AUTOSK_BIN`. The MCP
  server's `task` / `comment` tools shell out to it (same requirement
  `@autosk/pi-tools` imposes); a missing binary returns a clear actionable error
  rather than a silent failure.

## How it works

On each `onRun` (task mode) the agent:

1. spawns
   `claude -p --output-format stream-json --input-format stream-json --verbose
   --include-partial-messages --replay-user-messages` (with the role's `model` /
   permission posture / extra args), registering **one stdio `autosk` MCP server**
   via an inline `--mcp-config` that re-invokes [`autoskd mcp`](../../../docs/daemon.md#the-autoskd-mcp-tool-server)
   (the binary path from `$AUTOSKD_BIN` / `process.execPath`). The model sees
   `mcp__autosk__transit`, `mcp__autosk__task`, and `mcp__autosk__comment`; those
   three tools are auto-added to `--allowedTools` so a headless `acceptEdits` run
   never silently aborts on a permission prompt for them. The spawn env (and the
   `--mcp-config` env block) carry `AUTOSK_CWD` (the canonical project root, from
   `ctx.projectRoot`), `AUTOSK_AGENT` (the step name), and `AUTOSK_SOCK` (the
   running daemon's socket), so every `autosk` call resolves the task's own
   project, attributes comments to the step, and joins **this** daemon — even
   when the run is in a throwaway worktree;
2. seeds Claude with the rendered step prompt (role first-message + task context +
   the available transitions + "call `mcp__autosk__transit`");
3. mirrors Claude's `assistant` / `user` stream entries (text / thinking /
   tool_use / tool_result) into the autosk transcript 1:1 (see
   [`src/wire.ts`](src/wire.ts)), so existing pi-format renderers stay reusable;
   while a turn streams it also coalesces `stream_event` deltas into a cumulative
   assistant snapshot forwarded via `ctx.partial(m)` (~40 ms), so a client renders
   the in-progress message live before the durable line commits (see [docs/daemon.md →
   Streaming partial messages](../../../docs/daemon.md#streaming-partial-messages));
4. **observes the `mcp__autosk__transit` tool call** on Claude's stream and
   translates it into `ctx.transit(...)`. The MCP `transit` tool only *acks*; the
   driver — not the MCP server — drives the real transition (parity with
   pi-agent's `autosk_transit`). `task` / `comment` calls are **not** special-cased:
   they execute for real in the MCP server and flow through as ordinary
   tool_use / tool_result transcript lines;
5. runs a **kickback/corrections loop** (private to this extension): if a turn
   ends without a transit — or a chosen transition is rejected by the workflow's
   `onTransit` — it feeds a corrective message back to the model and retries, up
   to `maxCorrections` times. After the budget is spent it returns without a
   transit and the engine parks the task (`agent_did_not_transit`).

`onSteer` forwards a `session.input` message into the **live** Claude — idle → a
fresh turn; mid-turn → a stream-json `interrupt` control request followed by the
message. `onFollowup` does the same when idle, and **queues after the current
turn** when one is streaming (Claude's stream-json input has no mid-turn
follow-up command, only `interrupt`). `onAbort` asks Claude to wind down
gracefully (the engine's abort signal already terminates the child).

## Interactive (chat) mode

Besides backing a workflow step, this package's **default export** registers a
named agent, `"@autosk/claude-agent"`, via `autosk.registerAgent(...)`, so the
daemon can open an **interactive (taskless) chat session** against it (see
[docs/daemon.md → Interactive sessions](../../../docs/daemon.md#interactive-taskless-sessions)).
`onRun` branches on `ctx.mode`:

- `"task"` — the workflow transit loop above (unchanged).
- `"interactive"` — a chat loop: spawn `claude -p` stream-json with the `autosk`
  MCP server but **without** the `transit` tool (transit is unavailable in a
  chat, so it is not advertised), send **no** initial prompt (the session is
  empty until the user types), wire turn boundaries to `ctx.setActivity`
  (busy / idle), then await `ctx.signal`. Each composer message arrives via
  `onFollowup` and is forwarded to the live Claude. The agent returns when the
  signal fires; the engine seals the session `done` (graceful end), `aborted`
  (abort), or `failed` (crash) — no transit, no park.

## Configuration — `ClaudeAgentOptions`

The agent name is **not** an option — it is the workflow step key the
`claudeAgent` is assigned to (taken from `ctx.workflows.current.step` at run
time).

| Option                       | Default                          | Description                                                                                  |
| ---------------------------- | -------------------------------- | -------------------------------------------------------------------------------------------- |
| `model`                      | Claude Code default              | Model alias/name, e.g. `"sonnet"` / `"opus"` (`--model`).                                     |
| `effort`                     | Claude Code default              | Effort level (`--effort`): `low` / `medium` / `high` / `xhigh` / `max` (levels depend on the model). |
| `firstMessage`               | `""`                             | Inline first-message seed (wins over `firstMessageFile`).                                     |
| `firstMessageFile`           | —                                | Path to a file whose contents seed the first message.                                         |
| `appendSystemPrompt`         | —                                | Role guidance appended to the system prompt (`--append-system-prompt`).                       |
| `permissionMode`             | `"acceptEdits"`                  | Unattended permission mode (`--permission-mode`); must be non-interactive-safe.               |
| `dangerouslySkipPermissions` | `false`                          | Skip all permission prompts (`--dangerously-skip-permissions`); wins over `permissionMode`.   |
| `allowedTools`               | `[]`                             | Auto-approved tools (`--allowedTools`); the autosk MCP tools are added automatically.         |
| `disallowedTools`            | `[]`                             | Denied tools (`--disallowedTools`).                                                           |
| `bare`                       | `false`                          | `--bare` for hermetic runs (skip project `CLAUDE.md` / `.mcp.json` / hooks discovery).        |
| `autoskTools`                | `true`                           | Register the `autosk` MCP server. `false` omits `--mcp-config` (no transit → the run parks).  |
| `extraArgs`                  | `[]`                             | Extra args forwarded verbatim to `claude`.                                                    |
| `maxCorrections`             | `3`                              | Corrective turns before giving up (then the engine parks the task).                           |
| `claudeBin`                  | `$AUTOSK_CLAUDE_BIN` or `"claude"` | `claude` binary to spawn (the e2e tests point this at a stub).                              |

## The `autosk` MCP server

The tool surface is one stdio MCP server, shipped as the self-contained
[`autoskd mcp`](../../../docs/daemon.md#the-autoskd-mcp-tool-server) subcommand
(no `@modelcontextprotocol/sdk` dependency, so it bundles cleanly into the
compiled `autoskd` binary). It advertises up to three tools:

- **`transit`** — *ack-only*, gated by `AUTOSK_MCP_TRANSIT=1` (set only in task
  mode). The tool returns an immediate ack; the **driver** observes the
  `tool_use` on Claude's stream and drives the real `ctx.transit`, feeding any
  `onTransit` rejection back as a corrective message.
- **`task`** — `create` / `update` / `show` / `list`, the direct analog of
  `@autosk/pi-tools`' `autosk_task` (schemas + descriptions match it verbatim).
- **`comment`** — `add` / `list`, the analog of `@autosk/pi-tools`'
  `autosk_comment`; `add` defaults the author to `AUTOSK_AGENT`.

`task` / `comment` **execute for real** by shelling out to `autosk … --json` —
the CLI is the single place that already centralizes the project (`AUTOSK_CWD`),
socket (`AUTOSK_SOCK`), and author (`AUTOSK_AGENT`) resolution, so the server
inherits them from the `--mcp-config` env block and joins the running daemon
(no embedded RPC client, no second auto-spawned daemon).

## Project resolution under isolation

Under worktree isolation the agent runs in `~/.autosk/worktrees/<slug>/<task>`,
which contains no `.autosk/`. The daemon sets **`AUTOSK_CWD`** (= `ctx.projectRoot`)
in the spawned Claude's environment and in the `--mcp-config` env; the `autosk`
CLI honors it as the project selector, so task/comment calls resolve the original
project instead of walking up from the worktree. **`AUTOSK_SOCK`** (the running
daemon's socket) is propagated too, so the CLI joins **this** daemon rather than
auto-spawning a second one.

## Exports

- `claudeAgent(options)` → `AgentDefinition`
- `buildClaudeCommand(options, { mcpConfig?, interactive? })`,
  `buildMcpConfig(ctx, { transit })`, `autoskEnv(ctx)`, `resolveAutoskdBin()`,
  `ClaudeDriver`, `Coalescer`, `parseTarget`, `stripAnsi`, `TRANSIT_TOOL_NAME`
- the wire mappers (`mapAssistant`, `mapUser`, `mapResultUsage`, `mapStopReason`,
  `mapUsage`) and the prompt renderers (`renderInitialPrompt`, `kickbackMessage`,
  `rejectionMessage`, `targetLabel`, `targetLabels`) — exported for tooling / tests.
- default export — an extension factory that registers the named
  `"@autosk/claude-agent"` agent for interactive chat sessions. (Workflow roles
  are still registered separately, by the consuming extension, as inline
  `claudeAgent({...})` step values.)
