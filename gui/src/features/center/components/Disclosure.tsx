// Disclosure — a small presentational collapsible (plan §4.1). A clickable
// header row (rotating caret + header content + optional trailing badge) over a
// body shown only when `open`. Controlled: the parent owns the in-memory
// expand state and the live/error default logic. Plain <button> for keyboard
// a11y (`aria-expanded`); no <details>, no Tailwind — styles in transcript.css.

import type { ReactNode } from "react";

export function Disclosure({
  open,
  onToggle,
  header,
  badge,
  className,
  children,
}: {
  open: boolean;
  onToggle: () => void;
  header: ReactNode;
  /** Optional trailing status chip (e.g. running… / error). */
  badge?: ReactNode;
  className?: string;
  children?: ReactNode;
}) {
  return (
    <div className={`msg msg-disclosure${className ? ` ${className}` : ""}`}>
      <button type="button" className="msg-disclosure-toggle" aria-expanded={open} onClick={onToggle}>
        <span className={`msg-disclosure-caret${open ? " open" : ""}`} aria-hidden="true">
          ▶
        </span>
        <span className="msg-disclosure-header">{header}</span>
        {badge}
      </button>
      {open && <div className="msg-disclosure-body">{children}</div>}
    </div>
  );
}
