// Menu — a lightweight portal-rendered dropdown anchored to a button rect
// (redesign plan §8.5). Portaled to <body> so it is never clipped by a panel's
// overflow; a fixed backdrop closes it on outside click. Reused by the task row
// kebab and (Phase 6) the project switcher.

import { useEffect } from "react";
import { createPortal } from "react-dom";
import type { CSSProperties, ReactNode } from "react";

export function Menu({
  open,
  anchor,
  onClose,
  align = "right",
  children,
}: {
  open: boolean;
  anchor: DOMRect | null;
  onClose: () => void;
  align?: "left" | "right";
  children: ReactNode;
}) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open || !anchor) return null;
  const style: CSSProperties =
    align === "left"
      ? { position: "fixed", top: anchor.bottom + 4, left: anchor.left, zIndex: 1001 }
      : { position: "fixed", top: anchor.bottom + 4, right: Math.max(8, window.innerWidth - anchor.right), zIndex: 1001 };
  return createPortal(
    <>
      <div className="menu-backdrop" onMouseDown={onClose} />
      <div className="menu" style={style} role="menu" onMouseDown={(e) => e.stopPropagation()}>
        {children}
      </div>
    </>,
    document.body,
  );
}

export function MenuItem({
  onClick,
  danger,
  disabled,
  children,
}: {
  onClick: () => void;
  danger?: boolean;
  disabled?: boolean;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      role="menuitem"
      className={`menu-item${danger ? " menu-item-danger" : ""}`}
      disabled={disabled}
      onClick={onClick}
    >
      {children}
    </button>
  );
}

export function MenuLabel({ children }: { children: ReactNode }) {
  return <div className="menu-label">{children}</div>;
}

export function MenuDivider() {
  return <div className="menu-divider" />;
}
