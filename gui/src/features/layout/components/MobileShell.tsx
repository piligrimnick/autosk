// MobileShell — the compact (phone) single-pane shell (iPhone compact-layout
// plan). Re-hosts the EXISTING store-driven components in a one-level-deep,
// full-screen navigation. No new state: the list-vs-detail split is derived
// straight from `selection.kind`.
//
//   list root (selection.kind === "none"):
//     MobileTopBar / NoticeBar / the one active list panel / MobileTabBar
//   detail (selection.kind !== "none"):
//     MobileTopBar (Back + title) / NoticeBar / CenterPanel (+Composer) — the
//     tab bar is hidden so the composer owns the bottom edge.
//
// The hosted list panels carry their own ＋ / ↻ actions and create/browse
// modals; the detail's in-view actions (Abort/End/Enroll) live in the view
// headers. Settings opens the same modal as on desktop (ui.modal flag); the
// other modals render from inside the re-hosted panels / ProjectSwitcher and
// ConfirmDialog from its provider, so they all work unchanged here.

import { useEffect, useState } from "react";
import { useStore } from "@/state/store";
import { NoticeBar } from "@/components/NoticeBar";
import { TasksPanel } from "@/features/tasks/components/TasksPanel";
import { SessionsPanel } from "@/features/sessions/components/SessionsPanel";
import { WorkflowsPanel } from "@/features/workflows/components/WorkflowsPanel";
import { CenterPanel } from "@/features/center/components/CenterPanel";
import { SettingsModal } from "@/features/settings/components/SettingsModal";
import { MobileTopBar } from "./MobileTopBar";
import { MobileTabBar } from "./MobileTabBar";

/** The single list panel matching the active tab (ui.sidebarPanel). */
function ActiveList() {
  const { state } = useStore();
  switch (state.ui.sidebarPanel) {
    case "sessions":
      return <SessionsPanel />;
    case "workflows":
      return <WorkflowsPanel />;
    default:
      return <TasksPanel />;
  }
}

export function MobileShell() {
  const { state, effects } = useStore();
  const detail = state.selection.kind !== "none";
  const keyboardOpen = useViewportKeyboard();
  const closeModal = () => effects.openModal(null);

  return (
    <div
      className={`mobile-shell${detail ? " is-detail" : " is-list"}${keyboardOpen ? " is-keyboard" : ""}`}
    >
      <MobileTopBar />
      <NoticeBar />
      <div className="mobile-body">{detail ? <CenterPanel /> : <ActiveList />}</div>
      {!detail && <MobileTabBar />}
      {state.ui.modal === "settings" && <SettingsModal onClose={closeModal} />}
    </div>
  );
}

// Track the on-screen keyboard via the visual viewport. iOS WebKit (Safari /
// the Tauri WKWebView) ignores the `interactive-widget=resizes-content` viewport
// hint, so the keyboard OVERLAYS the `100dvh` shell instead of resizing it —
// leaving the composer marooned above the keyboard with dead space, and the
// scroll body cut off too early. We drive the shell height off
// `visualViewport.height` (published as the `--app-vh` custom property the shell
// height reads) so the shell always fits the area above the keyboard, and flag
// the keyboard-open state so the composer can drop its home-indicator padding
// (which the keyboard now covers). Returns whether the keyboard is currently up.
function useViewportKeyboard(): boolean {
  const [keyboardOpen, setKeyboardOpen] = useState(false);
  useEffect(() => {
    const vv = window.visualViewport;
    if (!vv) return;
    const root = document.documentElement;
    const apply = () => {
      root.style.setProperty("--app-vh", `${Math.round(vv.height)}px`);
      // The layout viewport (window.innerHeight) does NOT shrink for the iOS
      // keyboard, so a large innerHeight−visualViewport gap means it is up.
      setKeyboardOpen(window.innerHeight - vv.height > 80);
      // Keep the document pinned to the top: WKWebView scrolls the focused
      // field into view (and adds a keyboard contentInset) on its own scroll
      // view, which would otherwise drag the top bar off-screen. All real
      // scrolling lives in inner overflow panes, so the window stays at 0.
      if (window.scrollX !== 0 || window.scrollY !== 0) window.scrollTo(0, 0);
    };
    apply();
    vv.addEventListener("resize", apply);
    vv.addEventListener("scroll", apply);
    return () => {
      vv.removeEventListener("resize", apply);
      vv.removeEventListener("scroll", apply);
      root.style.removeProperty("--app-vh");
    };
  }, []);
  return keyboardOpen;
}
