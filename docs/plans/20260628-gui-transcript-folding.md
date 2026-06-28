# GUI transcript folding — collapsible thinking & tool blocks

**Status:** implemented (GUI-only, per scope below)
**Date:** 2026-06-28
**Scope (this iteration):** Tauri GUI only (`gui/src`). No daemon, SDK, wire, or
Go changes — the existing transcript data already carries everything we need.
The `autosk lazy` TUI is explicitly out of scope for this iteration.

## 1. Goal

Make the session transcript easier to scan by folding the two noisiest block
types:

- **Thinking blocks** are collapsed by default, showing only a one-line preview
  (`thinking › here goes thinking text from the model …`). Clicking the header
  toggles expand/collapse; expanded shows the full thinking text.
- **Tool calls** become their own collapsible block (today they render inside the
  `thinking`/assistant container with no folding). Collapsed shows only the tool
  name in the header. Expanded shows the call **arguments** and, separately, the
  tool **result**. The block must handle the in-progress state where the call
  exists in the transcript but the final result has not arrived yet.

## 2. Current behavior (what we're changing)

All transcript rendering lives in
`gui/src/features/center/components/Transcript.tsx`; styles in
`gui/src/styles/transcript.css`. Data flow is already fully event-driven
(`gui/src/services/ipc.ts` → `gui/src/state/reducer.ts` →
`gui/src/features/center/views/SessionView.tsx`); **no data-layer changes are
needed.**

Relevant facts about the data model (`daemon/sdk/src/transcript.ts`, mirrored in
`gui/src/types.ts`):

- An `AssistantMessage.content` is an array of `TextContent | ThinkingContent |
  ToolCall` blocks. `ContentBlockView` (Transcript.tsx) renders each one. Today
  `thinking` → `.msg-thinking` and `toolCall` → `.msg-tool`, both fully expanded,
  both inside the assistant bubble.
- A **tool result** is a *separate* top-level transcript line
  (`ToolResultMessage`, `role:"toolResult"`), rendered by `ToolResultRow`. It
  links back to its call via `toolResult.toolCallId === toolCall.id`, and carries
  `toolName`, `content` (text/image blocks), and `isError`.
- A live (in-progress) turn streams via the ephemeral `partial` assistant
  message (`SessionView` passes `partial` to `<Transcript>`; it renders as a
  trailing `AssistantRow … live`). A committed assistant line supersedes the
  partial (reducer `session/transcriptAppended` + `clearPartial`).

So a tool call lives inside an assistant message, while its result is a later
standalone line. To show "arguments + result in one block" we must **correlate
across lines by `toolCallId`**.

## 3. Decisions (locked)

These were confirmed with the requester:

1. **Merge call + result into one block.** Correlate `toolCall` with its
   `toolResult` by `toolCallId`. The standalone result row disappears (the result
   renders inside the expanded tool block). Orphan results (no matching call)
   keep their own fallback row.
2. **Pending state = animated `running…` indicator** in the tool block header
   when the call has no matching result yet (live execution).
3. **Thinking auto-expands while live, collapses on commit.** In the streaming
   `partial` row, thinking blocks render expanded so the operator sees the live
   text; once the durable assistant line commits, the block defaults to
   collapsed.
4. **Collapsed thinking preview = prefix + first non-empty line**, truncated with
   an ellipsis (e.g. `thinking › here goes thinking text …`).
5. **Expand/collapse state is in-memory**, scoped to the open session/view. It
   survives live streaming updates but resets on session switch / app restart.
   No persistence layer.
6. **Error tool results are collapsed by default**, with a red `error` badge in
   the header (not auto-expanded).
7. **Long content shows in full** — no inner max-height/scroll; only the outer
   transcript container scrolls.
8. **Rich per-tool header** (inspired by `pi-wierd-stuff`'s `facelift`, see §8):
   the collapsed tool header is a per-tool *summary* — tool name + primary
   argument + muted hints (e.g. `read /path (20 lines)`, `bash <cmd>`,
   `grep "pat" in /path`). **This supersedes the earlier "name-only" header.**
9. **Per-tool formatter registry, core set, textual only.** A registry keyed by
   tool name with formatters for `read`, `bash`, `grep`, `find`, `ls`, `write`,
   `edit`, `web_search`, `web_fetch`, plus a generic fallback. **No syntax
   highlighting and no diff rendering this iteration** — `write`/`edit` results
   show as plain text (rich diff/highlight are follow-ups, §10).
10. **No footer/result summary line.** Status is surfaced only via the header
    badge (`running…` / `error`); no `✓ exit 0 · 3 lines · 0.5s`-style footer.

## 4. Design

### 4.1 New components (all in `gui/src/features/center/components/`)

A small, reusable disclosure primitive plus two block components. No external
deps; plain `useState` + CSS (consistent with the rest of the GUI — no Tailwind,
no `<details>`).

- **`Disclosure`** (new file `Disclosure.tsx`) — a presentational collapsible:
  a clickable header row (caret + children) and a body shown only when open.
  Props: `{ open, onToggle, header, children, className? }`. Controlled by the
  parent so the parent owns the in-memory state and the live/error default
  logic. Header is a `<button>` for keyboard/ a11y (`aria-expanded`).

- **`ThinkingBlock`** (in Transcript.tsx or its own file) — wraps `Disclosure`.
  - Collapsed header: `thinking › <firstLinePreview>` using a `previewLine()`
    helper (first non-empty line of `block.thinking`, trimmed + ellipsis).
  - Expanded body: full `block.thinking` (reuse `.msg-dim` / `pre-wrap`).
  - `defaultOpen = live` (decision 3). The `live` flag is threaded from
    `AssistantRow`'s `live` prop down through `ContentBlockView`.

- **`ToolBlock`** (in Transcript.tsx or its own file) — wraps `Disclosure`,
  merges the call and its (optional) result.
  - Props: `{ call: ToolCall, result?: ToolResultMessage, live?: boolean }`.
  - Header (decision 8): a **per-tool rich summary** built by the formatter
    registry (§8) — `<toolName>` (bold) + primary arg (accent) + muted hints —
    followed by a status badge:
    - no `result` → `running…` (animated; reuse the `msg-live-cursor`
      blink keyframe family).
    - `result.isError` → red `error` badge.
    - otherwise → no badge (decision 10: no footer summary, no duration).
  - `defaultOpen = false` always (decision 6: even errors stay collapsed).
  - Expanded body, two labelled sections:
    1. **Arguments** — the formatter's `renderArgs(call.arguments)`, defaulting
       to `safeJson(call.arguments)` in `.msg-code`.
    2. **Result** — if `result`: the formatter's `renderResult(result)`,
       defaulting to the text/image join (reuse the `ToolResultRow` logic),
       styled error if `isError`. If no `result` yet: a muted
       `waiting for result…` placeholder.

### 4.2 Correlating results into calls (Transcript.tsx)

In the top-level `Transcript` component, before rendering:

1. Build `resultsByCallId: Map<string, ToolResultMessage>` by scanning every
   `lines[i]` that is a `MessageEntry` with `message.role === "toolResult"`.
2. Build `consumedCallIds: Set<string>` = every `toolCall.id` that appears in any
   assistant message (committed lines **and** the `partial`) — i.e. every call we
   will actually render a `ToolBlock` for.
3. Pass `resultsByCallId` down to `AssistantRow → ContentBlockView → ToolBlock`
   (via props or a small React context to avoid prop-drilling churn).
4. In `MessageRow`, **skip** a `toolResult` line whose `toolCallId` is in
   `consumedCallIds` (its content now lives in the merged `ToolBlock`). A
   `toolResult` with no matching call still renders via the existing
   `ToolResultRow` (orphan fallback).

This keeps ordering intuitive: the result visually folds up into the call block
inside the assistant bubble. When a tool call is committed before its result
arrives (common during live runs), the `ToolBlock` first shows `running…`; the
later `toolResult` append re-renders `Transcript`, the map gains the entry, and
the same (still-mounted) `ToolBlock` picks up the result — its open/closed state
is preserved because it's local `useState` keyed by the stable line id.

### 4.3 In-memory expand state (decision 5)

Use local `useState(defaultOpen)` inside each `ThinkingBlock` / `ToolBlock`.
Because transcript lines are keyed stably (`lineKey` = `${type}:${id}`), block
components stay mounted across streaming appends, so a user's manual toggle
survives live updates. Switching sessions remounts the subtree (the
`SessionView` transcript is rebuilt per session id), which resets to defaults —
exactly the desired scope. No Redux/store changes.

Note on the live→commit transition for thinking: the `partial` row (key
`partial-live`) and the eventual committed line are *different* React subtrees,
so a partial thinking block defaults open (live) and its committed counterpart
defaults closed — matching decision 3 with no extra bookkeeping.

### 4.4 Styling (`gui/src/styles/transcript.css`)

Add disclosure classes reusing existing tokens (`--bg*`, `--fg-mute`,
`--border`, `--radius`, `--accent`, `--mono`):

- `.msg-disclosure` (container), `.msg-disclosure-toggle` (clickable header,
  reuse `.msg-role` sizing + a caret that rotates on open),
  `.msg-disclosure-body`.
- Tool header badges: `.msg-tool-badge` with `--running` (animated, reuse
  `@keyframes msg-live-blink`) and `--error` (red, reuse `.msg-error` border
  color / a red text token) variants.
- Section labels inside the tool body: `Arguments` / `Result` small captions.
- Keep `.msg-code` for arguments + result text (already `pre-wrap`,
  `word-break`), per decision 7 (no inner max-height).

## 5. Files touched

- `gui/src/features/center/components/Transcript.tsx` — add result-correlation
  pre-pass; thread `resultsByCallId` + `live`; replace inline `thinking`/`toolCall`
  rendering with `ThinkingBlock`/`ToolBlock`; skip consumed `toolResult` rows.
- `gui/src/features/center/components/Disclosure.tsx` *(new)* — reusable
  collapsible primitive. (`ThinkingBlock`/`ToolBlock` may live here or stay in
  Transcript.tsx; keep them colocated with the renderer.)
- `gui/src/features/center/components/toolFormatters.tsx` *(new)* — the per-tool
  formatter registry (§8): pure summary helpers + optional `renderArgs` /
  `renderResult`, keyed by tool name, with a generic fallback.
- `gui/src/styles/transcript.css` — disclosure + tool-badge + tool-header styles.
- No changes to `types.ts`, `ipc.ts`, `events.ts`, reducers, or selectors.

## 6. Edge cases

- **Call with no result yet** → `running…` badge, `waiting for result…` body.
- **Result with no call** (orphan) → existing `ToolResultRow` fallback, unchanged.
- **Multiple results for one call** (shouldn't happen) → map keeps the last;
  acceptable.
- **Empty/whitespace-only thinking** → preview falls back to bare `thinking`
  label.
- **Image content in results** → reuse the existing `[mime image]` placeholder.
- **Partial tool calls** → tool blocks in the live `partial` row always render
  `running…` (results only arrive as committed lines).

## 7. Testing & checks

- `npm run typecheck` (tsc + the IPC-discipline guard — unaffected, no new
  `invoke`/`listen`).
- `npm run test` (vitest). Add pure unit tests for the helpers:
  `previewLine()` (first-line truncation), the result-correlation
  (`resultsByCallId` / `consumedCallIds`), and each per-tool `summary()`
  formatter (§8) — all pure functions, covered without a DOM. A light
  component-render test for `ToolBlock` (running vs done vs error) is optional;
  existing tests are logic-only.
- `npm run build` for the production bundle.
- Manual: run a live session, confirm thinking auto-expands while streaming and
  collapses on commit; confirm a tool block shows `running…` then folds in its
  result; confirm error badge + collapsed-by-default.

## 8. Per-tool formatting registry (inspired by `pi-wierd-stuff`/`facelift`)

`~/me/dev/pi-wierd-stuff` (`packages/facelift/index.ts` +
`packages/common/tool-frame/index.ts`) renders each tool with its own
`renderCall()` (a rich header summary: `tool name` bold + primary arg in accent
+ muted hints) and `renderResult()` (a tool-specific body), with status-driven
chrome (`pending`=warning / `success`=green / `error`=red). That is a terminal
(ANSI) renderer, but the *structure* ports cleanly to our React/CSS GUI. We
adopt the **header-summary + status + per-tool body** idea; we defer the
ANSI-specific parts (Shiki highlighting, diff frames, Nerd-Font icons, live
timers) to follow-ups (§10).

### 8.1 Why this is feasible with our data

Autosk's `ToolCall` carries `name` + `arguments` and `ToolResultMessage` carries
`toolName`, `content`, `isError`, **and `details?: unknown`** (the same
structured payload pi tools emit, e.g. `{ _type: "bashResult", exitCode,
command }`). So a formatter can build a rich summary from `arguments` and, when
present, read `details` for extra context — falling back to `content` text when
`details` is absent (non-pi agents). No wire/SDK change needed.

### 8.2 The registry contract

A new `toolFormatters.tsx` exposes a registry keyed by tool name (lowercased),
plus a generic fallback. Formatters are mostly **pure functions** (easy to unit
test); `renderArgs`/`renderResult` are optional React overrides.

```ts
interface ToolFormatter {
  /** Collapsed-header summary shown after the bold tool name. Pure + testable.
   *  Return the primary arg (accent) + muted hints, e.g. "/path (20 lines)". */
  summary(args: Record<string, unknown>): { primary?: string; hint?: string };
  /** Optional custom expanded args body. Default: safeJson(args) in .msg-code. */
  renderArgs?(args: Record<string, unknown>): React.ReactNode;
  /** Optional custom expanded result body. Default: text/image join. */
  renderResult?(result: ToolResultMessage): React.ReactNode;
}

function formatterFor(name: string): ToolFormatter; // registry lookup or fallback
```

`ToolBlock` composes it: header = `<bold name> <accent primary> <muted hint>` +
status badge; expanded body = `renderArgs(...)` + `renderResult(...)`.

### 8.3 Core-set summaries (decision 9)

Mirroring `facelift`'s `renderCall` titles, textual only:

| Tool | primary (accent) | hint (muted) |
|------|------------------|--------------|
| `read` | `args.path` | `from line N` / `(M lines)` from offset/limit |
| `bash` | first line of `args.command` (truncated) | — (multi-line elided) |
| `grep` | `"args.pattern"` | `in args.path` / `(args.glob)` |
| `find` | `args.pattern` | `in args.path` |
| `ls` | `args.path` | — |
| `write` | `args.path` | — |
| `edit` | `args.path` | `(N edits)` from `args.edits.length` |
| `web_search` | `"args.query"` | flags: `+Nd`/`-Nd`/`max=N`/country |
| `web_fetch` | `args.url` or `N pages` | `args.prompt` (truncated) |
| *fallback* | — (name only) | — |

Result bodies stay plain this iteration: the formatter's `renderResult` defaults
to the existing text/image join (the `ToolResultRow` logic), in `.msg-code`. The
only tool-specific result touch we may add cheaply is surfacing `bash`'s exit
code (from `details._type === "bashResult"`) as a small inline note — optional.

### 8.4 Status chrome mapping

`facelift`'s `FrameStatus` (`pending`/`success`/`error`) maps to our CSS:

- `pending` (no result yet) → `running…` badge + warning-accent header rule
  (reuse `--accent` / a warning token), reusing `@keyframes msg-live-blink`.
- `error` (`result.isError`) → red `error` badge + `.msg-error` border accent.
- `success` → neutral chrome, no badge (decision 10: no footer summary).

This stays additive over §4.4's disclosure styles — just a small set of
`.msg-tool-header` accent variants keyed on status.

## 9. Changelog

Per repo policy this is a user-visible GUI change — add an `### Added` (or
`### Changed`) bullet under `## [Unreleased]` in `CHANGELOG.md`, ≤15 words, e.g.
"GUI: transcript thinking and tool-call blocks now fold (collapsed by default)."

## 10. Out of scope / follow-ups

- `autosk lazy` TUI transcript folding (could mirror this later).
- Persisting expand state across restarts (decision: in-memory only).
- **Rich tool bodies** (deferred from §3 decision 9): syntax-highlighting for
  `read`/`grep` output and split/unified **diff rendering** for `write`/`edit`
  (as `pi-wierd-stuff`'s `facelift` does via Shiki + a diff package). This
  iteration keeps those bodies plain text.
- **Footer/result summary line** (deferred from decision 10): `✓ exit 0 · N
  lines · 0.5s`-style summaries + per-tool counts + (approx) duration.
- File-type icons / Nerd-Font glyphs for `ls`/`find` entries.
- Global "expand all / collapse all" control, copy-to-clipboard on blocks —
  possible future polish.
