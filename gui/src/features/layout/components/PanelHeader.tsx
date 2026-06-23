// PanelHeader — shared panel header chrome (redesign plan §5). Title on the
// left, optional actions on the right; styled by .panel-header in shell.css.
// When `onActivate` is supplied the whole header is a click target that expands
// the sidebar accordion panel; action buttons stop propagation so they never
// double-fire as an activate. `active` marks the currently-expanded panel.

import type { ReactNode } from "react";

export function PanelHeader({
  title,
  actions,
  className,
  onActivate,
  active,
}: {
  title: ReactNode;
  actions?: ReactNode;
  className?: string;
  onActivate?: () => void;
  active?: boolean;
}) {
  const classes = ["panel-header", onActivate ? "is-clickable" : "", active ? "is-active" : "", className ?? ""]
    .filter(Boolean)
    .join(" ");
  return (
    <div
      className={classes}
      onClick={onActivate}
      role={onActivate ? "button" : undefined}
      aria-expanded={onActivate ? active : undefined}
    >
      <span className="panel-title">{title}</span>
      {actions && (
        <div className="panel-header-actions" onClick={(e) => e.stopPropagation()}>
          {actions}
        </div>
      )}
    </div>
  );
}
