// SidebarResizer — the draggable divider on the sidebar's right edge. While
// dragging it writes the live `--sidebar-width` CSS var straight onto the
// panels container (so the heavy Tasks/Sessions/Workflows lists don't re-render
// on every pointer frame) and commits the final clamped width to the store on
// pointer-up (which the store persists to localStorage). Double-click resets to
// the default width.

import { useCallback, type PointerEvent as ReactPointerEvent, type RefObject } from "react";
import { SIDEBAR_DEFAULT_WIDTH, clampSidebarWidth } from "@/state/types";

interface Props {
  /** The `.app-panels` grid container that owns the `--sidebar-width` var. */
  containerRef: RefObject<HTMLDivElement | null>;
  /** Commit the final (already-clamped) width to the store. */
  onCommit: (width: number) => void;
}

export function SidebarResizer({ containerRef, onCommit }: Props) {
  const onPointerDown = useCallback(
    (e: ReactPointerEvent<HTMLDivElement>) => {
      // Ignore non-primary buttons; let double-click reset run on its own path.
      if (e.button !== 0) return;
      const container = containerRef.current;
      if (!container) return;
      e.preventDefault();

      const left = container.getBoundingClientRect().left;
      const widthAt = (clientX: number) => clampSidebarWidth(clientX - left);

      document.body.classList.add("is-resizing");

      const onMove = (ev: PointerEvent) => {
        container.style.setProperty("--sidebar-width", `${widthAt(ev.clientX)}px`);
      };
      const onUp = (ev: PointerEvent) => {
        document.body.classList.remove("is-resizing");
        window.removeEventListener("pointermove", onMove);
        window.removeEventListener("pointerup", onUp);
        onCommit(widthAt(ev.clientX));
      };

      window.addEventListener("pointermove", onMove);
      window.addEventListener("pointerup", onUp);
    },
    [containerRef, onCommit],
  );

  return (
    <div
      className="sidebar-resizer"
      role="separator"
      aria-orientation="vertical"
      aria-label="Resize sidebar"
      title="Drag to resize · double-click to reset"
      onPointerDown={onPointerDown}
      onDoubleClick={() => onCommit(SIDEBAR_DEFAULT_WIDTH)}
    />
  );
}
