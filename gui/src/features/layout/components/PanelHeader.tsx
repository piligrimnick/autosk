// PanelHeader — shared panel header chrome (redesign plan §5). Title on the
// left, optional actions on the right; styled by .panel-header in shell.css.

import type { ReactNode } from "react";

export function PanelHeader({
  title,
  actions,
  className,
}: {
  title: ReactNode;
  actions?: ReactNode;
  className?: string;
}) {
  return (
    <div className={`panel-header${className ? ` ${className}` : ""}`}>
      <span className="panel-title">{title}</span>
      {actions && <div className="panel-header-actions">{actions}</div>}
    </div>
  );
}
