// features/layout/utils/uiScale.ts — whole-UI zoom scale for the desktop app.
//
// Mirrors CodexMonitor's webview-zoom model: a single scalar applied to the
// entire webview via Tauri's `setZoom`, clamped to a sane range and persisted
// in localStorage. Like the sidebar geometry, the UI scale is layout-only UI
// state (not part of the daemon's domain), so it lives in the browser rather
// than the project DB. Pure + browser-safe so the unit tests can import it.

export const UI_SCALE_MIN = 0.5;
export const UI_SCALE_MAX = 3;
export const UI_SCALE_STEP = 0.1;
export const UI_SCALE_DEFAULT = 1;

const LS_UI_SCALE = "autosk.uiScale";

/** Round to the nearest step and clamp into [MIN, MAX]; NaN/∞ → default. */
export function clampUiScale(value: number): number {
  if (!Number.isFinite(value)) return UI_SCALE_DEFAULT;
  const rounded = Math.round(value / UI_SCALE_STEP) * UI_SCALE_STEP;
  const clamped = Math.min(UI_SCALE_MAX, Math.max(UI_SCALE_MIN, rounded));
  return Number(clamped.toFixed(2));
}

/** Human-readable percentage label, e.g. 1.1 → "110%". */
export function formatUiScale(value: number): string {
  return `${Math.round(clampUiScale(value) * 100)}%`;
}

/** Read the persisted scale (clamped); default when absent/unreadable. */
export function loadUiScale(): number {
  if (typeof window === "undefined") return UI_SCALE_DEFAULT;
  try {
    const raw = window.localStorage.getItem(LS_UI_SCALE);
    if (raw == null) return UI_SCALE_DEFAULT;
    return clampUiScale(Number(raw));
  } catch {
    return UI_SCALE_DEFAULT;
  }
}

/** Persist the scale (clamped). No-op outside the browser / on quota errors. */
export function saveUiScale(value: number): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(LS_UI_SCALE, String(clampUiScale(value)));
  } catch {
    /* private mode / quota — non-fatal */
  }
}
