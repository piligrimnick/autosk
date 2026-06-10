// ComposerInput — the chat-style input shared by the comment and steer
// composers. A single-line textarea that auto-grows with its content up to 10
// lines (then scrolls), with the send button (↑) pinned to the bottom-right
// corner so it keeps its bottom/right padding as the field grows.
//
// Keyboard: Cmd/Ctrl+Enter submits; a plain Enter inserts a newline.

import { useLayoutEffect, useRef, useState } from "react";

const MAX_LINES = 10;

export function ComposerInput({
  placeholder,
  onSubmit,
  disabled = false,
  sendTitle = "Send",
}: {
  placeholder: string;
  onSubmit: (text: string) => Promise<void> | void;
  disabled?: boolean;
  sendTitle?: string;
}) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const ref = useRef<HTMLTextAreaElement>(null);

  // Auto-grow: reset to the natural height, then clamp to MAX_LINES; toggle the
  // scrollbar only once the content overflows the cap.
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.style.height = "auto";
    const cs = getComputedStyle(el);
    const line = parseFloat(cs.lineHeight) || 18;
    const padY = parseFloat(cs.paddingTop) + parseFloat(cs.paddingBottom);
    const border = parseFloat(cs.borderTopWidth) + parseFloat(cs.borderBottomWidth);
    const max = Math.round(line * MAX_LINES + padY + border);
    const next = Math.min(el.scrollHeight + border, max);
    el.style.height = `${next}px`;
    el.style.overflowY = el.scrollHeight + border > max ? "auto" : "hidden";
  }, [text]);

  const submit = async () => {
    const value = text.trim();
    if (!value || busy || disabled) return;
    setBusy(true);
    try {
      await onSubmit(value);
      setText("");
    } finally {
      setBusy(false);
    }
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void submit();
    }
  };

  const blocked = disabled || busy;
  return (
    <div className="composer-input-wrap">
      <textarea
        ref={ref}
        className="composer-input"
        rows={1}
        placeholder={placeholder}
        value={text}
        disabled={disabled}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKeyDown}
      />
      <button
        type="button"
        className="composer-send"
        title={sendTitle}
        aria-label={sendTitle}
        disabled={blocked || !text.trim()}
        onClick={() => void submit()}
      >
        <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
          <path
            d="M8 13V4M8 4L4.5 7.5M8 4l3.5 3.5"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.8"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </button>
    </div>
  );
}
