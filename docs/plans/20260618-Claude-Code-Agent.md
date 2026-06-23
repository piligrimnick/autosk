# Claude Code Agent (`@autosk/claude-agent`) — Implementation Plan

Status: draft (design agreed via Q&A 2026-06-18)

Add a second shipped agent, `@autosk/claude-agent`, that drives **Claude Code**
(`claude -p` headless **stream-json** mode) as an autoskd v2 agent — the exact
analog of [`@autosk/pi-agent`](../../daemon/extensions/pi-agent/README.md), but
with Claude Code as the harness instead of `pi --mode rpc`. It returns an
`AgentDefinition` the engine runs for a workflow step (and registers a named
agent for interactive chat), satisfying the same contract pi-agent does.

## 1. Agreed decisions

Locked in before planning (do not re-litigate without revisiting):

1. **Driving mode → headless `stream-json`.** The daemon owns the child's stdio:
   spawn `claude -p --output-format stream-json --input-format stream-json
   --verbose --include-partial-messages` and read the NDJSON event stream
   directly, the way pi-agent reads `pi --mode rpc`. This is the robust path; it
   is **not** terminal-scraping. (`~/me/dev/fya` — driving the interactive TUI
   over a hidden PTY and tailing the transcript JSONL — was the alternative we
   rejected for v1; see §12 for the tmux/interactive backend as a future option.)
2. **Tool surface → one stdio `autosk` MCP server** via `--mcp-config`,
   exposing the full agent toolset (transit + task + comment), not just transit.
   The model sees `mcp__autosk__transit`, `mcp__autosk__task`,
   `mcp__autosk__comment`. The tools split into **two mechanics** (see §5):
   - **`transit`** is *session-scoped* and **observed**: the tool only acks, and
     the driver observes its `tool_use` on the output stream → `ctx.transit(...)`
     (an `onTransit` rejection is fed back as a corrective message). Structural
     twin of `pi-transit-extension.ts`.
   - **`task` / `comment`** are *project-store* operations the MCP server
     **executes for real** by **shelling out to `autosk … --json`** (returning
     the created task id, the task, the comment list, …) — the direct analog of
     `@autosk/pi-tools`, reusing its tool schemas/descriptions + argv/JSON layer
     verbatim. The `autosk` CLI is the single place that already handles the env
     nuances (`AUTOSK_CWD` project selector, `AUTOSK_SOCK` resolution, daemon
     connect-or-auto-spawn, author attribution), so we never re-implement them.
3. **Deliverable → this design doc first.** No code until the plan is agreed.

Everything else (kickback loop, prompt rendering, the Coalescer, `AUTOSK_CWD` /
`AUTOSK_AGENT` env, interactive chat mode, isolation behavior) ports from
pi-agent with minimal change.

## 2. What we build on (the agent contract is unchanged)

The engine drives any agent through `AgentDefinition`
(`daemon/sdk/src/agent.ts`): `onRun(ctx)` runs one step in-process and MUST call
`ctx.transit(...)` exactly once (or, in `interactive` mode, run a chat loop and
return on `ctx.signal`). The context gives us everything we need with no engine
change:

- `ctx.spawn(cmd, {cwd, env})` → a `ChildHandle` with line-buffered
  `onStdout`/`onStderr`, a `stdin` writer, `kill()`, and `exited`. This is what
  pi-agent uses; we reuse it verbatim to run `claude`.
- `ctx.log.message(m)` / `ctx.log.custom(t, d)` → the pi-format transcript.
- `ctx.partial(m)` → ephemeral live snapshots (coalesced ~40 ms).
- `ctx.transit(target)` → validate via `workflow.onTransit`, then commit once.
- `ctx.tasks` / `ctx.workflows` → task + current `{ workflow, step, targets }`.
- `ctx.projectRoot` / `ctx.cwd` → canonical project vs run dir (worktree).
- `ctx.signal` → abort / shutdown; `ctx.setActivity(...)` → busy/idle (chat).

**Nothing in `daemon/core` or `daemon/sdk` needs to change.** This is a pure new
extension package, exactly like pi-agent. (The one shared dependency we touch is
the build/test wiring that lists the workspace extensions — §10.)

## 3. Package layout (`daemon/extensions/claude-agent/`)

Mirror pi-agent's structure (`index.ts` root re-export shim → `src/`):

```
daemon/extensions/claude-agent/
  index.ts                      # root re-export (compiled-daemon resolver shim)
  package.json                  # @autosk/claude-agent, exports "./index.ts"
  tsconfig.json
  README.md
  src/
    index.ts                    # claudeAgent(opts) factory + default registerAgent
    driver.ts                   # ClaudeDriver: stream-json wire ↔ PiDriverHooks-shaped hooks
    wire.ts                     # Claude event → pi-format TranscriptMessage mapping
    prompt.ts                   # re-export / thin wrapper over pi-agent's prompt renderers
    mcp-server.ts               # the stdio `autosk` MCP server (transit + task + comment)
  test/
    driver.test.ts
    wire.test.ts
    mcp-server.test.ts
```

The `autosk` MCP server itself ships **inside the `autoskd` binary** as an
`autoskd mcp` subcommand (§5), not as a loose package file — `mcp-server.ts`
lives in `daemon/core/src` and is wired into the daemon's arg parser, so it is
self-contained on a clean machine. The extension package only builds the
`--mcp-config` that points Claude at `autoskd mcp`.

`package.json` copies pi-agent's verbatim, changing `name`/`description`/
`keywords`; same `"autosk": { "extensions": ["./index.ts"] }`, same
`"exports": { ".": "./index.ts" }`, same `@autosk/sdk` dependency, same
`typecheck` script. The root `index.ts` is the same shim pi-agent uses (the
compiled `autoskd` resolver only honors a root-level `index.ts`).

### Reuse vs. new code

| File | Source |
| --- | --- |
| `prompt.ts` | **reuse** pi-agent's `renderInitialPrompt` / `kickbackMessage` / `rejectionMessage` / `targetLabels` — they are harness-agnostic (task + targets + comments → text). Either import from `@autosk/pi-agent` or copy the file. Recommendation: **copy** (keeps the two agents independently versioned/publishable; the prompts are ~80 lines). |
| Coalescer | **reuse** pi-agent's `Coalescer<T>` (move it to a tiny shared spot or copy). |
| `index.ts` kickback loop + `liveSessions` + `autoskEnv` | **port** from pi-agent `runTurns`/`runChat`/`forward`/`autoskEnv` with the `ClaudeDriver` swapped in for `PiDriver`. |
| `driver.ts` | **new** — the Claude stream-json wire protocol. |
| `wire.ts` | **new** — Anthropic content blocks → pi-format message mapping. |
| `mcp-server.ts` (in `daemon/core`) | **new** — the stdio `autosk` MCP server (`autoskd mcp`); **reuses** `@autosk/pi-tools`' tool schemas/descriptions + its argv/JSON layer for `task`/`comment`. |

## 4. The driver (`src/driver.ts`)

`ClaudeDriver` plays the role `PiDriver` plays for pi: it wraps the `ChildHandle`
and exposes the same surface the kickback loop expects, so `src/index.ts` reads
almost identically to pi-agent's. Public surface (same shape as `PiDriver`):

```ts
class ClaudeDriver {
  constructor(child: ChildHandle, hooks: ClaudeDriverHooks);
  sendPrompt(message: string): Promise<void>;     // first turn / a corrective turn
  input(kind: "steer" | "followup", message: string): Promise<void>;
  waitForTurnEnd(): Promise<TurnEnd>;              // "ended" | "exited" | "aborted"
  takePendingTarget(): StepTarget | null;          // the observed mcp__autosk__transit target
  shutdown(graceMs?: number): Promise<void>;
  exitCode: number | null;
}
```

`ClaudeDriverHooks` is `PiDriverHooks` verbatim (`onMessage`, `onCustom`,
`signal`, `onActivity?`, `onPartial?`, `warn?`).

### 4.1 Spawn argv (`buildClaudeCommand`)

```
claude -p
  --output-format stream-json
  --input-format stream-json
  --verbose                       # required for stream-json
  --include-partial-messages      # token deltas → ctx.partial
  --replay-user-messages          # echo our stdin user msgs back in-order
  --mcp-config <inline-json>      # the autosk transit MCP server (task mode only)
  [--model <opts.model>]
  [--permission-mode <opts.permissionMode>] | [--dangerously-skip-permissions]
  [--allowedTools <csv>] [--disallowedTools <csv>]
  [--append-system-prompt <opts.appendSystemPrompt>]
  [--bare]                        # opt-in: reproducible, skip auto-discovery
  [...opts.extraArgs]
```

Notes:
- No positional prompt: with `--input-format stream-json` the prompt and every
  subsequent turn arrive as **user messages on stdin** (multi-turn in one
  long-lived process — the same model as pi's `prompt`/`follow_up`).
- `--mcp-config` is **omitted in interactive mode** (transit is not offered in a
  chat), exactly like pi-agent skips injecting `autosk_transit`.
- Permissions MUST be pre-resolved for an unattended run: a headless run that
  hits an un-allowed tool **aborts** (it cannot prompt). Default to
  `permissionMode: "acceptEdits"` with a sensible `allowedTools`, or
  `dangerouslySkipPermissions: true` under worktree isolation (the worktree is the
  sandbox). Make both options.
- `--bare` is opt-in (off by default) so project `CLAUDE.md` / `.mcp.json` / hooks
  still load by default; flip it on for hermetic CI-style runs.

### 4.2 Reading the stream (stdout, line-buffered NDJSON)

Each stdout line is one JSON event. Switch on `type`:

| Claude event | Driver action |
| --- | --- |
| `system` / `subtype:"init"` | record `session_id` (for `--resume`), model, mcp servers; `onActivity?(true)` is **not** fired here — the first turn's start is the prompt we sent. Optionally `warn` if the `autosk` MCP server failed to load. |
| `stream_event` (partial) | accumulate `content_block_delta` (`text_delta` / `thinking_delta`) into a **cumulative** assistant snapshot → push to the `Coalescer` → `onPartial`. (`message_start` opens a fresh snapshot; `content_block_start` for `tool_use` is reflected as a tool-call-in-progress block if we choose to surface it.) |
| `assistant` | map the full message → pi-format `AssistantMessage` (`wire.ts`), `flush()` the coalescer, `onMessage`. **Observe** any `tool_use` block whose `name === "mcp__autosk__transit"` → `parseTarget(input)` → `pendingTarget` (and remember `tool_use.id → name` for the matching `tool_result`). |
| `user` (tool results) | map each `tool_result` block → pi-format `ToolResultMessage` (`toolName` resolved from the remembered id→name map) → `onMessage`. |
| `result` | the **turn boundary** (pi's `agent_end`): `onActivity?(false)`, flush partials, `emitTurn("ended")`. `is_error:true` → also `warn` with `result`/`subtype`. |
| `system` / `subtype:"api_retry"` | `warn` (surface retry progress); not a turn end. |

Turn-boundary / pending-target / `waitForTurnEnd` / `emitTurn` queueing /
child-exit + abort teardown all copy pi-agent's `PiDriver` machinery
(`turnQueue`, `turnResolve`, `Coalescer.stop()` on exit/abort, resolve-pending on
exit). The differences from pi are purely the wire vocabulary (`assistant` /
`user` / `result` / `stream_event` instead of `message_end` / `agent_end` /
`message_update`), so the state machine and its tests transfer directly.

### 4.3 Sending input (stdin, stream-json user messages)

A turn is started by writing a user message line:

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<msg>"}]}}
```

- `sendPrompt(msg)` (first turn + every corrective/kickback turn) writes exactly
  this, then resolves. With `--replay-user-messages` the message is echoed back on
  stdout so it lands in the transcript in-order (we still mirror it ourselves via
  `onMessage` to keep parity if replay is disabled — pick one source of truth;
  recommendation: rely on `--replay-user-messages` and mirror the replayed `user`
  event, so the transcript matches exactly what Claude saw).
- `input("followup", msg)` while **idle** → same as `sendPrompt` (a new turn).
- `input("steer", msg)` while **streaming** → an **interrupt + message**: write
  a control request, then the user message:

  ```json
  {"type":"control_request","request":{"subtype":"interrupt"}}
  {"type":"user","message":{"role":"user","content":[{"type":"text","text":"<msg>"}]}}
  ```

  This mirrors pi-agent's `buildInputCommand` idle-vs-streaming dispatch and its
  retry-with-opposite-shape on a state-mismatch. **Open item to verify during
  impl** (§9): the exact control-message envelope the CLI's `--input-format
  stream-json` accepts for interrupt (the Agent SDK exposes `interrupt()`; the raw
  CLI control-request subtype must be confirmed against the installed `claude`
  version). If interrupt-mid-turn proves unsupported on the CLI, fall back to
  **queue-after-turn** (write the user message; it is consumed when the current
  turn ends) — still correct, just less immediate; document the degradation.

### 4.4 `shutdown` / abort

Copy `PiDriver.shutdown`: best-effort interrupt control-request (fire-and-forget),
close stdin, brief grace race against `child.exited`, then `child.kill()`.
Idempotent; called from both `onAbort` and `onRun`'s `finally`. The engine's
`ctx.signal` already terminates the child on abort/shutdown — this just lets
Claude wind a turn down first.

## 5. The `autosk` MCP server (`autoskd mcp`)

One stdio MCP server gives the model the full autosk toolset. It ships **inside
the `autoskd` binary** as the `autoskd mcp` subcommand (the compiled daemon is
self-contained — no global `bun`/`node` at runtime), registered with Claude via
an inline `--mcp-config`:

```jsonc
// inline value passed to --mcp-config
{
  "mcpServers": {
    "autosk": {
      "type": "stdio",
      "command": "<autoskd binary>",     // process.execPath (compiled) / $AUTOSKD_BIN (dev)
      "args": ["mcp"],
      "env": { "AUTOSK_CWD": "<projectRoot>", "AUTOSK_AGENT": "<step>", "AUTOSK_SOCK": "<daemon sock>" }
    }
  }
}
```

The driver resolves the binary path from the daemon's own `process.execPath`
(compiled) / `$AUTOSKD_BIN` (dev) and bakes it + the autosk env (§9) into the
inline config. `autoskd mcp` is a minimal stdio MCP server (JSON-RPC over stdio:
`initialize` → `tools/list` → `tools/call`) exposing up to three tools. They
split into **two mechanics**:

### 5.1 `transit` — session-scoped, **observed** (ack-only)

The direct analog of `pi-transit-extension.ts`. One string arg `to` (a sibling
step name, or `done` | `cancel` | `human`) — same schema and prompt guidance as
the pi tool. Exposed to the model as `mcp__autosk__transit`. The tool body **only
returns an ack**; the daemon's `ClaudeDriver` observes the `tool_use` on the
stream and drives the real `ctx.transit(...)`, so an `onTransit` rejection is fed
back to the model as a corrective follow-up (§6). It cannot be done by the MCP
server itself: transit is session-scoped (it needs the live `SessionRuntime`'s
`ctx.transit` + `onTransit` validation + the corrective loop), and the MCP server
is a separate process with no session handle. **Registered only in task mode**
(an env flag, e.g. `AUTOSK_MCP_TRANSIT=1`, that `runChat` does not set), so an
interactive chat is never offered a tool that has nothing to transit.
`parseTarget(input)` reuses pi-agent's logic (`{ to }` primary; `{ step }` /
`{ status }` accepted for robustness).

### 5.2 `task` / `comment` — project-store ops, **executed for real**

The analog of `@autosk/pi-tools`. `mcp__autosk__task`
(create/update/show/list) and `mcp__autosk__comment` (add/list) are **not**
session-scoped — they are plain operations on the project's `.autosk/` store, so
the MCP server runs them itself and returns the **real result** (the created task
id, the task object, the comment list) to the model. Reuse `@autosk/pi-tools`
verbatim where possible:

- **Tool schemas + descriptions + prompt-guidelines** — copy the `autosk_task` /
  `autosk_comment` JSON shapes and LLM-facing text from
  `pi-tools/src/{task,comment}.ts` (already battle-tested guidance). MCP
  `inputSchema` = the same `{ action, args }` object.
- **Execution — shell out to `autosk … --json`** (decided). `autoskd mcp` spawns
  the `autosk` CLI and parses its `--json` output, reusing `pi-tools/src/{argv,
  cli}.ts`' argv builders + JSON parsing verbatim. **Rationale:** the `autosk`
  CLI is the *single* place that already centralizes a long tail of nuances we
  must not re-implement — the `AUTOSK_CWD` project selector (walk-up vs override),
  `AUTOSK_SOCK` socket resolution, daemon **connect-or-auto-spawn**, the
  RFC3339/JSON wire format, `AUTOSK_AGENT` author attribution, and any future env
  knobs. An embedded proto-v2 RPC client would duplicate all of that and drift.
  Cost: `autosk` (the Go CLI) must be on `PATH` — the same requirement
  `@autosk/pi-tools` already imposes; document it, and honor an optional
  `$AUTOSK_BIN` override (else `"autosk"`). The pi-tools missing-binary heuristic
  (`looksLikeMissingBinary` → a clear "build it / put ./bin on PATH" error)
  ports over unchanged.

`comment add` defaults its author to `$AUTOSK_AGENT` (the step), already set in
the MCP server env (§9). These tools flow through Claude's normal
`tool_use`/`tool_result` events, so `wire.ts` mirrors them into the autosk
transcript with no special handling — only `transit` is special-cased on the
observe side. **Registered in both task and interactive mode** (creating/listing
tasks and commenting is useful in a chat too).

### 5.3 Alternative considered (not chosen for v1)

An in-process **HTTP/SSE** MCP server per session, whose `transit` handler calls
`ctx.transit` directly and returns the `onTransit` rejection text as a tool
error. Cleaner rejection ergonomics, but it needs a per-session localhost port +
a live handler↔session binding and would move the corrective loop out of the
agent extension into core; it breaks parity with the proven
"observe-the-tool-call + corrective message" pattern. A `session.transit` UDS-RPC
(now that `task`/`comment` already use an RPC channel) is a related option with
the same downside (the kickback loop lives in the agent's `onRun`, not core) and
adds a session-scoped RPC where the CLI shell-out deliberately keeps task/comment
on the existing `autosk --json` path. Keep both in reserve.

## 6. The kickback / corrections loop (`src/index.ts`)

Identical to pi-agent's `runTurns`, with `ClaudeDriver` swapped in:

1. render the initial prompt (`renderInitialPrompt`: role first-message + task +
   targets + comments) and `sendPrompt` it.
2. `waitForTurnEnd()`:
   - `"aborted"` → return (engine seals aborted);
   - `"exited"` → throw (`claude exited before recording a transition`);
   - `"ended"` → `takePendingTarget()`:
     - target present → `commitTransit`; success → return; rejection → if budget
       left, `sendPrompt(rejectionMessage(...))` and loop, else return (engine
       parks `agent_did_not_transit`).
     - no target → if budget left, `sendPrompt(kickbackMessage(...))` and loop,
       else return (park).

`maxCorrections` default `3`. `liveSessions` registry + `onSteer`/`onFollowup`/
`onAbort` forwarding copy pi-agent verbatim.

## 7. Interactive (chat) mode

Same branch as pi-agent: `onRun` checks `ctx.mode === "interactive"` → `runChat`:
spawn `claude` with the `autosk` MCP server but **without** the transit flag (so
the model gets `task`/`comment` but not `transit`, which has nothing to transit
in a chat — §5.1), send **no** initial prompt, register the driver before the
first `await`, wire `onActivity` →
`ctx.setActivity("busy"|"idle")`, then `await` the abort signal. Each composer
message arrives via `onFollowup` → `driver.input("followup", msg)` (idle → fresh
turn). The runtime seals `done`/`aborted`/`failed`; `onAbort` winds the driver
down. The default export registers the named agent:

```ts
export default function claudeAgentExtension(autosk: AutoskAPI): void {
  autosk.registerAgent({
    name: "@autosk/claude-agent",
    description: "system-wide Claude Code agent",
    agent: claudeAgent(),
  });
}
```

## 8. Options (`ClaudeAgentOptions`)

Parallels `PiAgentOptions`; harness-specific where Claude differs:

| Option | Default | Notes |
| --- | --- | --- |
| `model` | Claude default | `--model` (e.g. `"sonnet"`, `"opus"`). |
| `firstMessage` / `firstMessageFile` | `""` | first-message seed (file read once per process, as pi-agent does). |
| `appendSystemPrompt` | — | `--append-system-prompt` (role guidance). |
| `permissionMode` | `"acceptEdits"` | `--permission-mode` (`default`/`acceptEdits`/`dontAsk`/`plan`). |
| `dangerouslySkipPermissions` | `false` | `--dangerously-skip-permissions` (wins over `permissionMode`; intended for worktree isolation). |
| `allowedTools` / `disallowedTools` | — | `--allowedTools` / `--disallowedTools` (csv). |
| `bare` | `false` | `--bare` for hermetic runs. |
| `autoskTools` | `true` | register the `autosk` MCP server (§5). `false` → omit `--mcp-config` (no transit/task/comment tools; transit then never fires → the run always parks). |
| `extraArgs` | `[]` | forwarded verbatim. |
| `maxCorrections` | `3` | corrective turns before giving up → park. |
| `claudeBin` | `$AUTOSK_CLAUDE_BIN` or `"claude"` | binary to spawn (e2e tests point this at a stub). |

No `model:thinking` combo like pi — Claude's effort is a model/flag concern;
expose via `model` + `extraArgs` (e.g. `--thinking`/reasoning flags) until a
first-class option is warranted.

## 9. Env / isolation (unchanged from pi-agent)

`autoskEnv(ctx)` sets `AUTOSK_CWD = ctx.projectRoot`, `AUTOSK_AGENT =
ctx.workflows.current.step`, **and `AUTOSK_SOCK` = the running daemon's UDS
path** (new vs pi-agent), merged over `process.env` by `ctx.spawn`. The same env
is baked into the `--mcp-config` so the `autosk` CLI that `autoskd mcp` shells
out to (§5.2) resolves the right project (`AUTOSK_CWD`) and connects to the
*same* running daemon (`AUTOSK_SOCK`) instead of auto-spawning a second one —
letting the CLI handle that resolution is exactly the point of shelling out to it
rather than re-implementing it. (The daemon knows its own socket via
`daemon/core/src/rpc/paths.ts`; the Docker-isolation plan sets the same pair for
the in-container `autosk`.) Claude runs with `cwd = ctx.cwd` (the worktree under
isolation). Two Claude-specific caveats:

- **Transcript path under isolation.** Claude writes its own JSONL under
  `~/.claude/projects/<encoded-cwd>/`. We do **not** read it (we read stdout), so
  per-worktree cwd encoding is irrelevant to us — but note it so nobody wires a
  transcript tail by mistake.
- **`AUTOSK_CWD` for the `task`/`comment` tools.** `mcp__autosk__task` /
  `mcp__autosk__comment` (§5.2) resolve the project from `AUTOSK_CWD`, so they
  target the real project even when Claude runs in a worktree — exactly as
  pi-tools does for pi. Workflow transitions stay on the observed
  `mcp__autosk__transit` channel (§5.1).

## 10. Build / test wiring (the only non-extension touchpoints)

- The Bun workspace (`daemon/`) auto-includes the new package via the workspace
  globs; `bun install` symlinks it. Confirm `daemon/package.json` `workspaces`
  covers `extensions/*` (pi-agent/worktree/feature-dev are already there).
- `bun run typecheck` (root) must pick up the new `tsconfig.json`.
- `make build-autoskd`: `autoskd` learns the `mcp` subcommand (§5) in
  `daemon/core` — wire it in the daemon's arg parser (`daemon/core/src`) behind a
  non-default verb so the normal daemon path is untouched. The subcommand pulls
  in an MCP stdio server (hand-rolled JSON-RPC or `@modelcontextprotocol/sdk` —
  confirm it bundles cleanly under `bun build --compile`); `task`/`comment`
  **shell out to the `autosk` CLI** (§5.2), so no UDS client is added to the
  binary — but `autosk` must be on `PATH` (or `$AUTOSK_BIN`) at runtime, same as
  `@autosk/pi-tools`.
- **Bootstrap / availability.** Decide whether `@autosk/claude-agent` is:
  (a) published to npm and installed by `ensureGlobalBootstrap` like
  `@autosk/feature-dev` (so it is available everywhere), or (b) only used by a
  workflow that imports it. Recommendation: **publish it** (parity with pi-agent),
  and add a reference workflow (or a `--harness claude` variant of `feature-dev`)
  in a follow-up — out of scope for the agent package itself.

## 11. Risks / open items (verify during impl)

1. **Interrupt-mid-turn envelope** (§4.3) — confirm the CLI stream-json control
   message for interrupt against the installed `claude`; fall back to
   queue-after-turn if unsupported. (Highest-risk item for the steer story.)
2. **Per-message cost/usage** — Claude reports `usage` per assistant message and
   `total_cost_usd` at `result` level; pi-format `Usage` wants per-message cost.
   Map tokens faithfully; set per-message `cost` to `0`/derived and attribute the
   run total from the `result` event (or a `custom` entry). Don't block on exact
   cost parity.
3. **Tool-result `toolName` resolution** — `tool_result` blocks carry only
   `tool_use_id`; keep the id→name map from the preceding `assistant` to fill
   `ToolResultMessage.toolName` (built-in tools included, not just the MCP tool).
4. **`thinking` blocks** — map `thinking`/`redacted_thinking` content to
   `ThinkingContent`; partial `thinking_delta` into the cumulative snapshot.
5. **Headless permission aborts** — make the default permission posture
   non-interactive-safe (acceptEdits/skip) so a tool call never silently aborts
   the run; surface an aborting `result` via `warn`.
6. **stream-json input liveness** — confirm `claude -p --input-format stream-json`
   stays alive for multiple turns until stdin closes (the multi-turn model the
   kickback loop relies on). The docs describe streaming input as exactly this;
   verify with the stub + a live smoke.
7. **`task`/`comment` execution = `autosk --json` shell-out** (decided, §5.2).
   `autoskd mcp` spawns the `autosk` CLI (single place handling env/socket
   nuances). Confirm `autosk` is resolvable on `PATH` (or `$AUTOSK_BIN`) in the
   spawned claude/MCP env, and that `AUTOSK_SOCK` is propagated so the CLI joins
   the *running* daemon (not a fresh auto-spawn). Confirm the MCP stdio impl
   bundles under `bun build --compile` (no global `bun`/`node` at runtime).

## 12. Out of scope (explicit follow-ups)

- **tmux / interactive-TUI backend** (the human-attachable, fya-style driver):
  launch interactive `claude` in a named tmux window, drive via `send-keys`, read
  via transcript-JSONL tail, transit via the same MCP tool — selectable as a
  second `ClaudeAgentOptions.backend`. This is where the "run in tmux for
  convenient management / attach & intervene" idea lands; deferred until the
  robust headless backend is in.
- A reference workflow using the Claude agent (or `feature-dev --harness claude`).
- First-class thinking/effort option, model profiles.
- `--resume` based session continuation across runtime restarts (the driver
  already records `session_id` from `system/init`, so this is additive later).

## 13. Milestones

1. **Scaffold** `daemon/extensions/claude-agent/` (package.json, tsconfig, root
   shim, README); `bun install` + `bun run typecheck` green with stub `src`.
2. **`autoskd mcp`** subcommand in `daemon/core` + the `--mcp-config` plumbing:
   the `transit` (ack-only) tool first, then port `task` / `comment` from
   `@autosk/pi-tools` (schemas + `argv`/`cli` shell-out to `autosk --json`).
   A manual `tools/list` / `tools/call` smoke against `autoskd mcp` (assert
   `transit` acks, `task`/`comment` return real store results).
3. **`wire.ts`** mapping (Anthropic blocks → pi-format) with unit tests over
   captured `assistant` / `user` / `result` / `stream_event` fixtures.
4. **`driver.ts`** state machine (turn boundaries, pending target, partials,
   steer/abort) — port `PiDriver` tests to the Claude vocabulary; add a `claude`
   stub binary (à la pi-agent's e2e stub) keyed by `$AUTOSK_CLAUDE_BIN`.
5. **`index.ts`** factory + kickback loop + interactive `runChat` + default
   `registerAgent`; reuse `prompt.ts` + `Coalescer`.
6. **Docs + changelog**: `daemon/extensions/claude-agent/README.md`,
   a mention in `docs/workflows.md` / `docs/extensions.md`, and a `CHANGELOG.md`
   `[Unreleased]` → `### Added` entry (new `@autosk/claude-agent` agent).

## 14. Testing

- **`wire.test.ts`** — fixture-driven: each Claude event shape maps to the
  expected pi-format `TranscriptMessage` (text/thinking/tool_use/tool_result,
  usage, stopReason); the `mcp__autosk__transit` tool_use yields the right
  `StepTarget`.
- **`driver.test.ts`** — the same state-machine cases pi-agent's `PiDriver` tests
  cover: turn-end queueing vs await race, `takePendingTarget`, partial
  coalescing + `stop()` on exit/abort, steer idle-vs-streaming dispatch + retry,
  `shutdown` idempotency. Drive it with synthetic NDJSON lines (no real `claude`).
- **`mcp-server.test.ts`** — the `autoskd mcp` server: `initialize` /
  `tools/list` advertises `transit` + `task` + `comment` (transit gated by
  `AUTOSK_MCP_TRANSIT`); `tools/call transit` acks; `task`/`comment` shell out to
  a **fake `autosk` binary** (keyed by `$AUTOSK_BIN`) emitting canned `--json`
  and return the mapped result; argv/JSON shapes match pi-tools (reuse its
  `argv.test.ts` cases). Also assert the missing-binary error path.
- **e2e** (opt-in, like pi-agent's): a `claude` stub that emits a canned
  stream-json sequence ending in an `mcp__autosk__transit` tool_use → assert
  `ctx.transit` is called once; a variant with no transit → assert the kickback
  loop and eventual park.
- No `daemon/core` or `daemon/sdk` test changes (no contract change).
