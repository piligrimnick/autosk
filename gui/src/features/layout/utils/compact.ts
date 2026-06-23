// features/layout/utils/compact.ts — the pure activation predicate for the
// compact (phone) layout (iPhone compact-layout plan, decision 3C). A device is
// "compact" when it is a TOUCH device AND small enough — iPhone in portrait
// (width clause) or landscape (height clause) — so non-touch desktop windows
// (any width) and iPad (portrait + landscape) stay on the two-pane shell.
//
// Kept free of `matchMedia` so it is unit-testable under the node vitest env
// (matching the platform.ts pattern). The media-query string below is the
// single source of truth shared verbatim with `styles/mobile.css` and the
// `useIsCompact` hook, so CSS gating and JS mounting never disagree.

/** The activation media query — mirrored verbatim in styles/mobile.css. */
export const COMPACT_MEDIA_QUERY =
  "(pointer: coarse) and ((max-width: 700px) or (max-height: 480px))";

export interface CompactViewport {
  width: number;
  height: number;
  /** Whether the primary pointer is coarse (touch). */
  coarsePointer: boolean;
}

/**
 * Whether the compact single-pane layout should engage. Mirrors
 * COMPACT_MEDIA_QUERY: a coarse (touch) pointer AND (narrow OR short).
 *   - width <= 700  catches iPhone portrait; iPad portrait (768+) does not.
 *   - height <= 480 catches iPhone landscape; iPad landscape (768+ tall) does
 *     not, and an iPhone Pro Max in landscape (~932px wide) still matches.
 * A fine pointer (desktop mouse) never matches, no matter how narrow (3C).
 */
export function isCompactViewport({ width, height, coarsePointer }: CompactViewport): boolean {
  if (!coarsePointer) return false;
  return width <= 700 || height <= 480;
}
