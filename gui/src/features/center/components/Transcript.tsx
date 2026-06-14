// Transcript — the shared transcript renderer (redesign plan §8.3). Renders a
// session's pi-format transcript lines (`session.transcript` / `session-event`)
// oldest-first: a SessionHeader line, pi MessageEntry lines (user / assistant /
// toolResult — with text / thinking / toolCall / image content blocks), and the
// engine's structural `autosk:*` custom entries (transit / steer / error /
// session_end). Scroll/tail policy lives in the SessionView scroll container
// (useStickToBottom); this component just renders the lines oldest-first.

import type {
  AssistantMessage,
  ContentBlock,
  CustomEntry,
  MessageEntry,
  SessionHeader,
  ToolResultMessage,
  TranscriptLine,
  UserMessage,
} from "@/types";
import { isAutoskCustomType } from "@/types";
import { Markdown } from "@/components/Markdown";

export function Transcript({ lines }: { lines: TranscriptLine[] }) {
  return (
    <div className="transcript">
      {lines.map((line, idx) => (
        <TranscriptRow key={lineKey(line, idx)} line={line} />
      ))}
    </div>
  );
}

function lineKey(line: TranscriptLine, idx: number): string {
  if (line.type === "session") return `h:${line.id}`;
  return `${line.type}:${line.id ?? idx}`;
}

function TranscriptRow({ line }: { line: TranscriptLine }) {
  switch (line.type) {
    case "session":
      return <HeaderRow header={line} />;
    case "message":
      return <MessageRow entry={line} />;
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

function MessageRow({ entry }: { entry: MessageEntry }) {
  const m = entry.message;
  switch (m.role) {
    case "assistant":
      return <AssistantRow message={m} />;
    case "user":
      return <UserRow message={m} />;
    case "toolResult":
      return <ToolResultRow message={m} />;
    default:
      return null;
  }
}

function AssistantRow({ message }: { message: AssistantMessage }) {
  return (
    <div className="msg msg-assistant">
      <div className="msg-role">assistant</div>
      <div className="msg-body">
        {message.content.map((block, i) => (
          <ContentBlockView key={i} block={block} />
        ))}
      </div>
    </div>
  );
}

function UserRow({ message }: { message: UserMessage }) {
  return (
    <div className="msg msg-user">
      <div className="msg-role">user</div>
      <div className="msg-body">
        {typeof message.content === "string" ? (
          <Markdown text={message.content} />
        ) : (
          message.content.map((block, i) => <ContentBlockView key={i} block={block} />)
        )}
      </div>
    </div>
  );
}

function ToolResultRow({ message }: { message: ToolResultMessage }) {
  const text = message.content
    .map((b) => (b.type === "text" ? b.text : `[${b.mimeType} image]`))
    .join("\n");
  return (
    <div className={`msg msg-toolresult ${message.isError ? "msg-error" : ""}`}>
      <div className="msg-role">
        result · {message.toolName}
        {message.isError ? " · error" : ""}
      </div>
      {text && <pre className="msg-code">{text}</pre>}
    </div>
  );
}

/** Render one pi message content block (text / thinking / toolCall / image). */
function ContentBlockView({ block }: { block: ContentBlock }) {
  switch (block.type) {
    case "text":
      return <Markdown text={block.text} />;
    case "thinking":
      return (
        <div className="msg msg-thinking">
          <div className="msg-role">thinking</div>
          <div className="msg-body msg-dim">{block.thinking}</div>
        </div>
      );
    case "toolCall":
      return (
        <div className="msg msg-tool">
          <div className="msg-role">tool · {block.name}</div>
          <pre className="msg-code">{safeJson(block.arguments)}</pre>
        </div>
      );
    case "image":
      return <div className="msg-system-text msg-dim">[{block.mimeType} image]</div>;
    default:
      return null;
  }
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
