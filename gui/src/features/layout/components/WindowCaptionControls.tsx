// Windows caption controls (minimize / restore-maximize / close) for the
// frameless window (redesign plan §4.3). macOS keeps its native traffic lights
// via `titleBarStyle: Overlay`, so this renders only on Windows inside the
// Tauri runtime. Mirrors CodexMonitor's WindowCaptionControls, minus the
// lucide-react dependency (inline SVG icons instead).

import { useEffect, useState } from "react";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { isTauriRuntime, isWindowsPlatform } from "../utils/platform";

type WindowHandle = ReturnType<typeof getCurrentWindow>;

function currentWindowSafe(): WindowHandle | null {
  try {
    return getCurrentWindow();
  } catch {
    return null;
  }
}

export function WindowCaptionControls() {
  const isEnabled = isWindowsPlatform() && isTauriRuntime();
  const [isMaximized, setIsMaximized] = useState(false);

  useEffect(() => {
    if (!isEnabled) return;
    let mounted = true;
    let unlistenResized: (() => void) | null = null;
    const handle = currentWindowSafe();
    if (!handle) return;

    const sync = async () => {
      try {
        const next = await handle.isMaximized();
        if (mounted) setIsMaximized(next);
      } catch {
        /* non-Tauri / test runtime */
      }
    };

    void sync();
    void handle
      .onResized(() => {
        void sync();
      })
      .then((unlisten) => {
        if (!mounted) {
          unlisten();
          return;
        }
        unlistenResized = unlisten;
      })
      .catch(() => {
        /* ignore */
      });

    return () => {
      mounted = false;
      if (unlistenResized) unlistenResized();
    };
  }, [isEnabled]);

  if (!isEnabled) return null;

  const run = (fn: (handle: WindowHandle) => void) => {
    const handle = currentWindowSafe();
    if (handle) fn(handle);
  };

  return (
    <div className="window-caption-controls" role="group" aria-label="Window controls">
      <button
        type="button"
        className="window-caption-control"
        aria-label="Minimize window"
        data-tauri-drag-region="false"
        onClick={() => run((w) => void w.minimize())}
      >
        <IconMinimize />
      </button>
      <button
        type="button"
        className="window-caption-control"
        aria-label={isMaximized ? "Restore window" : "Maximize window"}
        data-tauri-drag-region="false"
        onClick={() => run((w) => void w.toggleMaximize())}
      >
        {isMaximized ? <IconRestore /> : <IconMaximize />}
      </button>
      <button
        type="button"
        className="window-caption-control window-caption-control-close"
        aria-label="Close window"
        data-tauri-drag-region="false"
        onClick={() => run((w) => void w.close())}
      >
        <IconClose />
      </button>
    </div>
  );
}

function IconMinimize() {
  return (
    <svg viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.2" aria-hidden>
      <line x1="2" y1="7" x2="12" y2="7" />
    </svg>
  );
}

function IconMaximize() {
  return (
    <svg viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.2" aria-hidden>
      <rect x="2.5" y="2.5" width="9" height="9" />
    </svg>
  );
}

function IconRestore() {
  return (
    <svg viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.2" aria-hidden>
      <rect x="2.5" y="4.5" width="7" height="7" />
      <path d="M4.5 4.5 V2.5 H11.5 V9.5 H9.5" />
    </svg>
  );
}

function IconClose() {
  return (
    <svg viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.2" aria-hidden>
      <line x1="3" y1="3" x2="11" y2="11" />
      <line x1="11" y1="3" x2="3" y2="11" />
    </svg>
  );
}
