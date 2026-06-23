// useStickToBottom — shared scroll policy for the center body's scroll
// containers (the session transcript and the task sheet). Two rules, matching
// the lazy TUI's winDetail behaviour:
//
//   1. When the rendered entity changes (`resetKey`), re-anchor the viewport
//      *instantly* (no animation): `resetTo: "bottom"` jumps to the newest line
//      — used when switching to a session in the Sessions panel; `resetTo:
//      "top"` jumps to the start — used when opening a task (tasks open from the
//      beginning, not the tail).
//   2. While the same entity stays selected, keep the viewport pinned to the
//      bottom as content grows ONLY when the user is already at (or near) the
//      bottom. New lines/comments are then followed with a *smooth* scroll
//      animation (instead of a jarring instant jump), so a live-tailing
//      transcript glides to the newest line. If the user has scrolled up to
//      read history, their position is left untouched when new content arrives.
//
// The smooth tail animation fires its own stream of `scroll` events; those must
// not be mistaken for the operator scrolling away from the bottom (which would
// drop us out of tail mode and stall the animation). We therefore guard the
// programmatic animation (`autoScrollingRef`) and only release the tail when an
// in-flight animation is *reversed* by a real upward scroll — so a streaming
// transcript never traps the reader, yet a stray downward jitter mid-animation
// keeps tailing.

import { useLayoutEffect, useRef } from "react";

// Treat the viewport as "at the bottom" within this many CSS px of the end, so
// sub-pixel rounding and small layout shifts don't drop us out of tail mode.
const AT_BOTTOM_THRESHOLD_PX = 32;

function distanceFromBottom(el: HTMLElement): number {
  return el.scrollHeight - el.scrollTop - el.clientHeight;
}

function isAtBottom(el: HTMLElement): boolean {
  return distanceFromBottom(el) <= AT_BOTTOM_THRESHOLD_PX;
}

// Honour the OS "reduce motion" setting: fall back to an instant jump so the
// tail still follows new content, just without the glide.
function tailBehavior(): ScrollBehavior {
  if (typeof window !== "undefined" && typeof window.matchMedia === "function") {
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return "auto";
  }
  return "smooth";
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
  // True while our own smooth tail animation is gliding to the bottom. Scroll
  // events fired by that animation must not flip us out of tail mode.
  const autoScrollingRef = useRef(false);
  // Last observed scrollTop, used to tell a downward tail animation apart from
  // a real upward (user-initiated) scroll while the animation is in flight.
  const lastTopRef = useRef(0);

  // Runs after every render (no dep array): re-anchor on entity change,
  // otherwise tail the bottom while the user is parked there. Reading
  // scrollHeight is gated on atBottomRef so scrolled-up history reading pays no
  // layout cost.
  useLayoutEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    if (resetKey !== lastResetKey.current) {
      lastResetKey.current = resetKey;
      autoScrollingRef.current = false;
      if (resetTo === "bottom") {
        el.scrollTop = el.scrollHeight; // instant anchor on open — never animate
        atBottomRef.current = true;
      } else {
        el.scrollTop = 0;
        atBottomRef.current = isAtBottom(el);
      }
      lastTopRef.current = el.scrollTop;
      return;
    }

    // Same entity, content may have grown: glide to the newest line while the
    // operator is parked at the bottom. Skip the animation when there is
    // nothing to cover so we never arm the guard without a follow-up scroll
    // event to disarm it.
    if (atBottomRef.current && distanceFromBottom(el) > 1) {
      autoScrollingRef.current = true;
      el.scrollTo({ top: el.scrollHeight, behavior: tailBehavior() });
    }
  });

  const onScroll = (e: React.UIEvent<HTMLDivElement>) => {
    const el = e.currentTarget;
    const prevTop = lastTopRef.current;
    const curTop = el.scrollTop;
    lastTopRef.current = curTop;

    if (autoScrollingRef.current) {
      // The tail animation only ever moves downward (scrollTop increases). A
      // net upward move means the operator grabbed the scrollbar / wheeled up —
      // release the tail and hand control back to them.
      if (curTop < prevTop - 1) {
        autoScrollingRef.current = false;
        atBottomRef.current = false;
        return;
      }
      // Reached the end → settle back into steady-state tail tracking.
      if (isAtBottom(el)) {
        autoScrollingRef.current = false;
        atBottomRef.current = true;
      }
      return;
    }

    atBottomRef.current = isAtBottom(el);
  };

  return { containerRef, onScroll };
}
