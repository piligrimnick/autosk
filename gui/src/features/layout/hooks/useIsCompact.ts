// features/layout/hooks/useIsCompact.ts — subscribes to the compact-layout
// activation media query and returns whether the compact (phone) shell should
// mount. The query string lives in utils/compact.ts so CSS, the hook, and the
// pure predicate all agree on the breakpoint.
//
// SSR/test-safe: returns `false` when `matchMedia` is unavailable (the node
// vitest env), matching the platform.ts / useStickToBottom.ts patterns. Built
// on useSyncExternalStore so a device rotation / window resize across the
// breakpoint re-renders the shell.

import { useSyncExternalStore } from "react";
import { COMPACT_MEDIA_QUERY } from "../utils/compact";

function getMql(): MediaQueryList | null {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return null;
  return window.matchMedia(COMPACT_MEDIA_QUERY);
}

function subscribe(onChange: () => void): () => void {
  const mql = getMql();
  if (!mql) return () => {};
  mql.addEventListener("change", onChange);
  return () => mql.removeEventListener("change", onChange);
}

function getSnapshot(): boolean {
  return getMql()?.matches ?? false;
}

/** True when the compact single-pane (phone) layout should be shown. */
export function useIsCompact(): boolean {
  // The server snapshot is `false` (two-pane) for SSR / non-browser test runs.
  return useSyncExternalStore(subscribe, getSnapshot, () => false);
}
