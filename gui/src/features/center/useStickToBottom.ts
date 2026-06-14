// useStickToBottom — shared scroll policy for the center body's scroll
// containers (the session transcript and the task sheet). Two rules, matching
// the lazy TUI's winDetail behaviour:
//
//   1. When the rendered entity changes (`resetKey`), re-anchor the viewport:
//      `resetTo: "bottom"` jumps to the newest line — used when switching to a
//      session in the Sessions panel; `resetTo: "top"` jumps to the start —
//      used when opening a task (tasks open from the beginning, not the tail).
//   2. While the same entity stays selected, keep the viewport pinned to the
//      bottom as content grows ONLY when the user is already at (or near) the
//      bottom. If they have scrolled up to read history, their position is
//      left untouched when new lines/comments arrive.

import { useLayoutEffect, useRef } from "react";

// Treat the viewport as "at the bottom" within this many CSS px of the end, so
// sub-pixel rounding and small layout shifts don't drop us out of tail mode.
const AT_BOTTOM_THRESHOLD_PX = 32;

function isAtBottom(el: HTMLElement): boolean {
  return el.scrollHeight - el.scrollTop - el.clientHeight <= AT_BOTTOM_THRESHOLD_PX;
}

export interface StickToBottom {
  /** Attach to the scroll container (`overflow-y: auto`). */
  containerRef: React.RefObject<HTMLDivElement>;
  /** Wire to the container's `onScroll` so user scrolls update the at-bottom flag. */
  onScroll: (e: React.UIEvent<HTMLDivElement>) => void;
}

export function useStickToBottom({
  resetKey,
  resetTo = "bottom",
}: {
  /** Identity of the entity in the pane; a change re-anchors the viewport. */
  resetKey: string | null;
  /** Where to land when `resetKey` changes. Defaults to "bottom". */
  resetTo?: "top" | "bottom";
}): StickToBottom {
  const containerRef = useRef<HTMLDivElement>(null);
  const atBottomRef = useRef(resetTo === "bottom");
  const lastResetKey = useRef<string | null>(null);

  // Runs after every render (no dep array): re-anchor on entity change,
  // otherwise tail the bottom while the user is parked there. Reading
  // scrollHeight is gated on atBottomRef so scrolled-up history reading pays no
  // layout cost.
  useLayoutEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    if (resetKey !== lastResetKey.current) {
      lastResetKey.current = resetKey;
      if (resetTo === "bottom") {
        el.scrollTop = el.scrollHeight;
        atBottomRef.current = true;
      } else {
        el.scrollTop = 0;
        atBottomRef.current = isAtBottom(el);
      }
      return;
    }

    if (atBottomRef.current) {
      el.scrollTop = el.scrollHeight;
    }
  });

  const onScroll = (e: React.UIEvent<HTMLDivElement>) => {
    atBottomRef.current = isAtBottom(e.currentTarget);
  };

  return { containerRef, onScroll };
}
