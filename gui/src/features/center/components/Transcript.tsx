// Transcript — the shared transcript renderer (redesign plan §8.3). Renders a
// session's pi-format transcript lines (`session.transcript` / `session-event`)
// oldest-first: a SessionHeader line, pi MessageEntry lines (user / assistant /
// toolResult — with text / thinking / toolCall / image content blocks), and the
// engine's structural `autosk:*` custom entries (transit / steer / error /
// session_end). Scroll/tail policy lives in the SessionView scroll container
// (useStickToBottom); this component just renders the lines oldest-first.
//
// Folding (plan docs/plans/20260628-gui-transcript-folding.md): `thinking` and
// `toolCall` blocks are collapsible. A pre-pass correlates each standalone
// `toolResult` line with its call by `toolCallId` so the result folds into the
// merged `ToolBlock` (the standalone row is then skipped); the index is shared
// via a small context to avoid prop-drilling. Expand state is in-memory
// (local useState), scoped to the mounted session subtree.

import { createContext, useContext, useMemo, useState } from "react";
import type {
  AssistantMessage,
  ContentBlock,
  CustomEntry,
  MessageEntry,
  SessionHeader,
  ThinkingContent,
  ToolCall,
  ToolResultMessage,
  TranscriptLine,
  TranscriptMessage,
  UserMessage,
} from "@/types";
import { isAutoskCustomType } from "@/types";
import { Markdown } from "@/components/Markdown";
import { Disclosure } from "./Disclosure";
import { formatterFor } from "./toolFormatters";
import { collectConsumedCallIds, indexResults, previewLine } from "./transcriptFolding";

/** Shares the call→result index down to each merged ToolBlock. */
const ResultsContext = createContext<Map<string, ToolResultMessage>>(new Map());

export function Transcript({
  lines,
  partial,
}: {
  lines: TranscriptLine[];
  /** Ephemeral in-progress assistant snapshot rendered as a trailing live row. */
  partial?: TranscriptMessage | null;
}) {
  // Correlation pre-pass (plan §4.2): index results by call id and collect the
  // call ids we will render as merged blocks (so their standalone result rows
  // are skipped). Recomputed only when the lines / partial change.
  const results = useMemo(() => indexResults(lines), [lines]);
  const consumed = useMemo(() => collectConsumedCallIds(lines, partial), [lines, partial]);

  return (
    <ResultsContext.Provider value={results}>
      <div className="transcript">
        {lines.map((line, idx) => (
          <TranscriptRow key={lineKey(line, idx)} line={line} consumed={consumed} />
        ))}
        {partial && partial.role === "assistant" && (
          <AssistantRow key="partial-live" message={partial} live />
        )}
      </div>
    </ResultsContext.Provider>
  );
}

function lineKey(line: TranscriptLine, idx: number): string {
  if (line.type === "session") return `h:${line.id}`;
  return `${line.type}:${line.id ?? idx}`;
}

function TranscriptRow({ line, consumed }: { line: TranscriptLine; consumed: Set<string> }) {
  switch (line.type) {
    case "session":
      return <HeaderRow header={line} />;
    case "message":
      return <MessageRow entry={line} consumed={consumed} />;
    case "custom":
      return <CustomRow entry={line as CustomEntry} />;
    default:
      return null;
  }
}

function HeaderRow({ header }: { header: SessionHeader }) {
  return (
    <div className="msg msg-system transcript-header">
      <span className="msg-chip">session</span>
      <span className="msg-system-text">
        {header.agent} · {header.workflow}:{header.step}
      </span>
    </div>
  );
}

function MessageRow({ entry, consumed }: { entry: MessageEntry; consumed: Set<string> }) {
  const m = entry.message;
  switch (m.role) {
    case "assistant":
      return <AssistantRow message={m} />;
    case "user":
      return <UserRow message={m} />;
    case "toolResult":
      // The result now folds into its merged ToolBlock; skip the standalone row
      // unless it is an orphan (no matching call) — then keep the fallback.
      if (consumed.has(m.toolCallId)) return null;
      return <ToolResultRow message={m} />;
    default:
      return null;
  }
}

function AssistantRow({ message, live = false }: { message: AssistantMessage; live?: boolean }) {
  return (
    <div className={`msg msg-assistant${live ? " msg-live" : ""}`}>
      <div className="msg-role">
        Assistant
        {live && (
          <span className="msg-live-cursor" aria-label="streaming" title="streaming">
            {" ▌"}
          </span>
        )}
      </div>
      <div className="msg-body">
        {message.content.map((block, i) => (
          <ContentBlockView key={i} block={block} live={live} />
        ))}
      </div>
    </div>
  );
}

function UserRow({ message }: { message: UserMessage }) {
  return (
    <div className="msg msg-user">
      <div className="msg-role">User</div>
      <div className="msg-body">
        {typeof message.content === "string" ? (
          <Markdown text={message.content} />
        ) : (
          message.content.map((block, i) => <ContentBlockView key={i} block={block} live={false} />)
        )}
      </div>
    </div>
  );
}

/** The text/image join used for a tool result body + the orphan fallback row. */
function toolResultText(message: ToolResultMessage): string {
  return message.content
    .map((b) => (b.type === "text" ? b.text : `[${b.mimeType} image]`))
    .join("\n");
}

function ToolResultRow({ message }: { message: ToolResultMessage }) {
  const text = toolResultText(message);
  return (
    <div className={`msg msg-toolresult ${message.isError ? "msg-error" : ""}`}>
      <div className="msg-role">
        Result · {message.toolName}
        {message.isError ? " · error" : ""}
      </div>
      {text && <pre className="msg-code">{text}</pre>}
    </div>
  );
}

/** Render one pi message content block (text / thinking / toolCall / image). */
function ContentBlockView({ block, live }: { block: ContentBlock; live: boolean }) {
  switch (block.type) {
    case "text":
      return <Markdown text={block.text} />;
    case "thinking":
      return <ThinkingBlock block={block} live={live} />;
    case "toolCall":
      return <ToolBlock call={block} live={live} />;
    case "image":
      return <div className="msg-system-text msg-dim">[{block.mimeType} image]</div>;
    default:
      return null;
  }
}

/** A collapsible thinking block: a one-line preview when collapsed, full text
 *  when expanded. Auto-expands while live (decision §3.3); committed defaults
 *  collapsed (a separate React subtree, so no extra bookkeeping). */
function ThinkingBlock({ block, live }: { block: ThinkingContent; live: boolean }) {
  const [open, setOpen] = useState(live);
  const preview = previewLine(block.thinking);
  const header = (
    <span className="msg-thinking-summary">
      <span className="msg-thinking-label">thinking</span>
      {preview && (
        <>
          <span className="msg-thinking-sep">›</span>
          <span className="msg-thinking-preview">{preview}</span>
        </>
      )}
    </span>
  );
  return (
    <Disclosure
      className="msg-thinking-block"
      open={open}
      onToggle={() => setOpen((v) => !v)}
      header={header}
    >
      <div className="msg-body msg-dim">{block.thinking}</div>
    </Disclosure>
  );
}

/** A collapsible tool block merging a call with its (optional) result. Header
 *  is a per-tool rich summary (registry §8) + a status badge. Collapsed by
 *  default — even errors (decision §3.6). A live partial row always shows the
 *  `running…` state; a committed call with no result yet shows it until the
 *  result line arrives and folds in (open state preserved across the append). */
function ToolBlock({ call, live }: { call: ToolCall; live: boolean }) {
  const results = useContext(ResultsContext);
  const result = live ? undefined : results.get(call.id);
  const [open, setOpen] = useState(false);
  const fmt = formatterFor(call.name);
  const { primary, hint } = fmt.summary(call.arguments ?? {});
  const status: "running" | "error" | "ok" = !result ? "running" : result.isError ? "error" : "ok";

  const header = (
    <span className="msg-tool-summary">
      <span className="msg-tool-name">{call.name}</span>
      {primary && <span className="msg-tool-primary">{primary}</span>}
      {hint && <span className="msg-tool-hint">{hint}</span>}
    </span>
  );
  const badge =
    status === "running" ? (
      <span className="msg-tool-badge running" aria-label="running">
        running…
      </span>
    ) : status === "error" ? (
      <span className="msg-tool-badge error">error</span>
    ) : null;

  return (
    <Disclosure
      className={`msg-tool-block${status === "error" ? " is-error" : ""}`}
      open={open}
      onToggle={() => setOpen((v) => !v)}
      header={header}
      badge={badge}
    >
      <div className="msg-tool-section">
        <div className="msg-tool-caption">Arguments</div>
        {fmt.renderArgs ? (
          fmt.renderArgs(call.arguments ?? {})
        ) : (
          <pre className="msg-code">{safeJson(call.arguments)}</pre>
        )}
      </div>
      <div className="msg-tool-section">
        <div className="msg-tool-caption">Result</div>
        {result ? (
          fmt.renderResult ? (
            fmt.renderResult(result)
          ) : (
            <ToolResultBody result={result} />
          )
        ) : (
          <div className="msg-dim msg-tool-waiting">waiting for result…</div>
        )}
      </div>
    </Disclosure>
  );
}

/** The default expanded result body: the text/image join in a code block,
 *  flagged with the error border when the result is an error. */
function ToolResultBody({ result }: { result: ToolResultMessage }) {
  const text = toolResultText(result);
  if (!text) return <div className="msg-dim msg-tool-waiting">(no output)</div>;
  return <pre className={`msg-code${result.isError ? " msg-error" : ""}`}>{text}</pre>;
}

/** Render a custom entry — the engine's structural `autosk:*` types and any
 *  generic agent custom logged via `ctx.log.custom`. */
function CustomRow({ entry }: { entry: CustomEntry }) {
  const label = entry.customType;
  const auto = isAutoskCustomType(label);
  const body = describeCustom(entry);
  return (
    <div className={`msg msg-system ${auto ? "msg-autosk" : ""}`}>
      <span className="msg-chip">{label.replace(/^autosk:/, "")}</span>
      {body && <span className="msg-system-text">{body}</span>}
    </div>
  );
}

function describeCustom(entry: CustomEntry): string {
  const data = entry.data as Record<string, unknown> | undefined;
  if (!data) return "";
  switch (entry.customType) {
    case "autosk:transit": {
      const to = data.to as { step?: string; status?: string } | undefined;
      return to ? `→ ${to.step ?? to.status ?? ""}` : "";
    }
    case "autosk:steer":
      return `${data.kind ?? "steer"}: ${String(data.message ?? "")}`;
    case "autosk:error":
      return String(data.error ?? data.message ?? "");
    case "autosk:session_end":
      return data.error ? `${data.status} · ${data.error}` : String(data.status ?? "");
    default:
      return safeJson(data);
  }
}

function safeJson(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
