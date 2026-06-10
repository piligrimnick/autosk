// features/layout/hooks/useUiScale.ts — whole-UI zoom (Cmd/Ctrl +, -, 0).
//
// The scale itself lives in the shared store (`state.ui.uiScale`, persisted to
// localStorage like the sidebar geometry) so the keyboard shortcuts and the
// Settings slider stay in lockstep. This hook is the side-effect layer: it
// applies the current scale to the webview via Tauri's `setZoom` and binds the
// standard browser-style zoom shortcuts, dispatching changes through the store.
//
// `@tauri-apps/api/webview` is loaded lazily and only inside the Tauri runtime,
// so outside it (web preview / vitest) the zoom call is a harmless no-op and
// the module never enters the test bundle. This is a distinct Tauri API from
// the JSON-RPC bridge, so it does not breach the single-`invoke`-site rule.
//
// `setZoom` only exists on desktop macOS/Windows (unsupported on Linux, iOS,
// Android), so the whole feature — apply + shortcuts, and the Settings slider —
// is gated on `isWebviewZoomSupported()`; elsewhere the hook is inert.

import { useEffect, useRef } from "react";
import { useStore } from "@/state/store";
import { isTauriRuntime, isWebviewZoomSupported } from "../utils/platform";
import { UI_SCALE_STEP, clampUiScale } from "../utils/uiScale";

async function applyWebviewZoom(scale: number): Promise<void> {
  if (!isTauriRuntime()) return;
  try {
    const { getCurrentWebview } = await import("@tauri-apps/api/webview");
    await getCurrentWebview().setZoom(scale);
  } catch {
    /* zoom is best-effort; a failure must never break the UI */
  }
}

/** Applies the store's UI scale to the webview and binds the zoom shortcuts. */
export function useUiScale(): void {
  const { state, effects } = useStore();
  const supported = isWebviewZoomSupported();
  const uiScale = state.ui.uiScale;

  // Keep the latest scale in a ref so the (stable) key handler reads fresh
  // values without re-binding the listener on every nudge.
  const scaleRef = useRef(uiScale);
  scaleRef.current = uiScale;

  // Apply to the webview whenever the scale changes (and once on mount).
  useEffect(() => {
    if (!supported) return;
    void applyWebviewZoom(uiScale);
  }, [supported, uiScale]);

  useEffect(() => {
    if (!supported) return;
    const onKeyDown = (event: KeyboardEvent) => {
      // Match CodexMonitor: only with the platform modifier, never with Alt.
      if (!event.metaKey && !event.ctrlKey) return;
      if (event.altKey) return;
      const key = event.key;
      const isIncrease = key === "+" || key === "=";
      const isDecrease = key === "-" || key === "_";
      const isReset = key === "0";
      if (!isIncrease && !isDecrease && !isReset) return;
      event.preventDefault();
      if (isReset) {
        effects.setUiScale(1);
      } else {
        effects.setUiScale(clampUiScale(scaleRef.current + (isDecrease ? -UI_SCALE_STEP : UI_SCALE_STEP)));
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [supported, effects]);
}
