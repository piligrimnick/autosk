// MobileTabBar — the compact bottom tab bar (iPhone compact-layout plan:
// navigation 1A + destinations 2A). Three tabs (Tasks / Sessions / Workflows)
// bound to `ui.sidebarPanel`; the active tab is highlighted. Tapping a tab
// switches the list AND clears the selection, so we always land on that list's
// root (never a stale pushed detail). Rendered by MobileShell only on the list
// root — the detail screen hides it so the composer owns the bottom edge.

import { useStore } from "@/state/store";
import type { SidebarPanel } from "@/state/types";

const TABS: { panel: SidebarPanel; label: string }[] = [
  { panel: "tasks", label: "Tasks" },
  { panel: "sessions", label: "Sessions" },
  { panel: "workflows", label: "Workflows" },
];

export function MobileTabBar() {
  const { state, effects } = useStore();
  const active = state.ui.sidebarPanel;
  // This is a bottom *navigation* bar (not a WAI-ARIA tabs widget: there are no
  // associated tabpanels), so it uses plain <nav> semantics with
  // aria-current="page" on the active tab rather than role=tablist/tab.
  return (
    <nav className="mobile-tabbar" aria-label="Sections">
      {TABS.map(({ panel, label }) => {
        const isActive = active === panel;
        return (
          <button
            key={panel}
            type="button"
            aria-current={isActive ? "page" : undefined}
            className={`mobile-tab${isActive ? " is-active" : ""}`}
            onClick={() => {
              // Switch the list AND drop any selection → land on the list root.
              effects.setSidebarPanel(panel);
              effects.clearSelection();
            }}
          >
            <span className="mobile-tab-icon" aria-hidden>
              <TabIcon panel={panel} />
            </span>
            <span className="mobile-tab-label">{label}</span>
          </button>
        );
      })}
    </nav>
  );
}

function TabIcon({ panel }: { panel: SidebarPanel }) {
  switch (panel) {
    case "tasks":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden>
          <path d="M4 6h11M4 12h11M4 18h7" strokeLinecap="round" />
          <path d="M18.5 15.5l1.6 1.6 3-3.4" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      );
    case "sessions":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden>
          <rect x="3.5" y="4.5" width="17" height="13" rx="2" />
          <path d="M7 21h10M12 17.5V21" strokeLinecap="round" />
          <path d="M7.5 8.5l2.5 2.2-2.5 2.2M11.5 13.5h3.5" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      );
    case "workflows":
      return (
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden>
          <rect x="3.5" y="3.5" width="6" height="6" rx="1.4" />
          <rect x="14.5" y="14.5" width="6" height="6" rx="1.4" />
          <path d="M9.5 6.5h4.5a3 3 0 0 1 3 3v5" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      );
  }
}
