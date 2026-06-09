// Transcript — the shared transcript renderer (redesign plan §8.3), extracted
// from the legacy TaskTimeline's MessageRow. Renders a job's message events
// oldest-first; assistant_* events render as markdown, the rest as compact
// labelled blocks. Auto-scrolls to the newest event as the transcript grows.

import { useEffect, useRef } from "react";
import type { MessageEvent } from "@/types";
import { Markdown } from "@/components/Markdown";

export function Transcript({ messages }: { messages: MessageEvent[] }) {
  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [messages.length]);

  return (
    <div className="transcript">
      {messages.map((event, idx) => (
        <MessageRow key={idx} event={event} />
      ))}
      <div ref={bottomRef} />
    </div>
  );
}

function MessageRow({ event }: { event: MessageEvent }) {
  switch (event.kind) {
    case "assistant_text":
      return (
        <div className="msg msg-assistant">
          <div className="msg-role">assistant</div>
          <div className="msg-body">
            <Markdown text={event.text ?? ""} />
          </div>
        </div>
      );
    case "assistant_thinking":
      return (
        <div className="msg msg-thinking">
          <div className="msg-role">thinking</div>
          <div className="msg-body msg-dim">{event.text}</div>
        </div>
      );
    case "user_text":
      return (
        <div className="msg msg-user">
          <div className="msg-role">user</div>
          <div className="msg-body">
            <Markdown text={event.text ?? ""} />
          </div>
        </div>
      );
    case "tool_call":
      return (
        <div className="msg msg-tool">
          <div className="msg-role">tool · {event.name}</div>
          {event.input != null && <pre className="msg-code">{safeJson(event.input)}</pre>}
        </div>
      );
    case "tool_result":
      return (
        <div className={`msg msg-toolresult ${event.is_error ? "msg-error" : ""}`}>
          <div className="msg-role">
            result{event.name ? ` · ${event.name}` : ""}
            {event.is_error ? " · error" : ""}
          </div>
          {event.text && <pre className="msg-code">{event.text}</pre>}
        </div>
      );
    default:
      // session / model_change / compaction / other — render a compact chip.
      return (
        <div className="msg msg-system">
          <span className="msg-chip">{event.kind}</span>
          {event.text && <span className="msg-system-text">{event.text}</span>}
        </div>
      );
  }
}

function safeJson(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
