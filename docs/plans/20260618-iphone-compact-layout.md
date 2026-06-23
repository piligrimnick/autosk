# iPhone (compact) layout for the autosk GUI

Status: **plan** (research complete; no code changed yet).
Date: 2026-06-18.

Make the Tauri GUI usable on an iPhone-class screen by adding a **compact,
full-screen, single-pane** layout that lives alongside the existing
desktop/iPad two-pane shell. The app already *installs* on iPhone — this is
purely a responsive-UI change.

## Locked decisions (from product review)

1. **Navigation pattern — bottom tab bar + push-to-detail** (option 1A).
   A persistent bottom tab bar (Tasks / Sessions / Workflows) switches the
   full-screen list; tapping a row pushes a full-screen detail with a ‹ Back
   button; the contextual composer is pinned at the bottom of the detail.
2. **Destinations — three lists primary, Settings = top-bar gear** (option 2A).
   Tasks, Sessions, Workflows are the three tabs; Settings stays a gear in the
   compact top bar (as it is today on desktop).
3. **Activation — width-based, but only on touch/iOS** (option 3C).
   The compact layout engages only on touch devices below the breakpoint;
   a non-touch desktop window stays two-pane no matter how narrow.

## Why this is a small, low-risk change

The existing state model already encodes everything the compact navigation
needs — **no new state, no new RPC, no reducer/selector changes**:

- `state.ui.sidebarPanel: "tasks" | "sessions" | "workflows"` → which **tab/list**
  is shown.
- `state.selection: task | session | workflow | none` → whether we're on the
  **list root** (`none`) or **pushed into detail** (non-`none`).
- `selectionSlice` (reducer.ts) already sets `sidebarPanel` to match a selected
  entity, so the correct tab auto-highlights when a notification selects
  something.
- Effects already exist: `setSidebarPanel(panel)`, `selectTask/selectSession/
  selectWorkflow`, `clearSelection()`.

So the compact UI is a **second presentation of the same store** — we re-host
the existing list panels, detail views (`CenterPanel`), and composer in a new
single-pane shell, and add a top bar + bottom tab bar.

The mobile groundwork is partly in place already: `shell.css` uses `100dvh`
and `env(safe-area-inset-*)`, `index.html` has `viewport-fit=cover`, and
`isWebviewZoomSupported()` already excludes iOS (so the Cmd/Ctrl zoom control
never appears on iPhone). There are currently **zero `@media` queries** in
`gui/src/styles/*` — this change introduces the first responsive rules.

## Current layout (what we're adapting)

```
AppShell (shell.css)
 ├─ Titlebar          ← sidebar-toggle, ProjectSwitcher, conn status, Settings, win caption controls
 ├─ NoticeBar
 └─ app-panels (grid: [sidebar-width] 1fr)
     ├─ sidebar-stack (accordion, active grows 3:1)
     │   ├─ TasksPanel      (PanelHeader "Tasks" + ＋new)
     │   ├─ SessionsPanel   (PanelHeader "Sessions" + ＋new + ↻)
     │   └─ WorkflowsPanel  (PanelHeader "Workflows" + ＋browse + ↻)
     ├─ SidebarResizer
     └─ CenterPanel
         ├─ center-body  → SessionView | TaskView | WorkflowView | EmptyView
         └─ Composer     (chat | steer | comment | none, by composerMode)
```

## Target compact layout

Two screens, one push level deep:

**List root** (`selection.kind === "none"`):
```
┌───────────────────────────┐
│ ‹proj▾›            ● ⚙     │  MobileTopBar (project switcher, conn dot, Settings)
├───────────────────────────┤
│  (active list, full-screen)│  the one panel matching ui.sidebarPanel,
│  • row …          + ↻      │  with its existing ＋ / ↻ actions in a section header
│  • row …                   │
├───────────────────────────┤
│ [ Tasks ][ Sessions ][ ⋯ ]│  MobileTabBar (3 tabs) — safe-area bottom inset
└───────────────────────────┘
```

**Detail** (`selection.kind !== "none"` — pushed):
```
┌───────────────────────────┐
│ ‹ Back   <title>     ⚙     │  MobileTopBar in "detail" mode (Back = clearSelection)
├───────────────────────────┤
│  CenterPanel detail body   │  reused verbatim (SessionView / TaskView / …)
│  …                         │  in-view actions (Abort/End/Enroll) stay in the body header
│ [ composer input … ]       │  Composer pinned at the bottom (no tab bar here)
└───────────────────────────┘
```

Key behaviors:
- **Tab tap** → `setSidebarPanel(panel)` **and** `clearSelection()` → always
  lands on that list's root.
- **Row tap** → `selectTask/Session/Workflow(...)` → flips `selection`,
  pushes detail (the existing effect already sets the matching `sidebarPanel`).
- **Back** → `clearSelection()` → returns to the list root for the current tab.
- **Tab bar is hidden on the detail screen** (iOS `hidesBottomBarWhenPushed`
  feel) so the composer owns the bottom edge — no tab-bar + composer stacking.
- The nav stack is exactly one level deep, fully derived from existing state;
  the hardware/edge Back gesture and our ‹ Back button both map to
  `clearSelection()`.

## Activation logic (decision 3C)

A device is "compact" when it is **touch** AND **small**. Expressed as one
media query reused by both CSS and the JS hook:

```
@media (pointer: coarse) and ((max-width: 700px) or (max-height: 480px))
```

- `pointer: coarse` ⇒ touch/iOS only; non-touch desktop never matches (3C).
- `max-width: 700px` ⇒ iPhone **portrait** (≤430px) matches; iPad portrait
  (768–834px) does **not** → iPad keeps two-pane (desired).
- `max-height: 480px` ⇒ iPhone **landscape** (height ~390–430px) also matches;
  iPad landscape (height 768–834px) does **not**. Without this clause, an
  iPhone Pro Max in landscape (~932px wide) would wrongly stay two-pane.

JS hook (drives which React tree mounts):

```ts
// features/layout/hooks/useIsCompact.ts
const MQ = "(pointer: coarse) and ((max-width: 700px) or (max-height: 480px))";
export function useIsCompact(): boolean { /* matchMedia(MQ) + change listener */ }
```

Keep the **pure decision** out of `matchMedia` so it's unit-testable:

```ts
// features/layout/utils/compact.ts
export function isCompactViewport(p: {
  width: number; height: number; coarsePointer: boolean;
}): boolean {
  return p.coarsePointer && (p.width <= 700 || p.height <= 480);
}
```

`useIsCompact` reads `window.matchMedia(MQ).matches` and subscribes to its
`change` event; SSR/test-safe (returns `false` when `matchMedia` is absent),
matching the existing `platform.ts` / `useStickToBottom.ts` patterns.

## Component inventory

**New (under `gui/src/features/layout/`):**

- `utils/compact.ts` — `isCompactViewport(...)` pure predicate (unit-tested).
- `hooks/useIsCompact.ts` — `matchMedia` subscription hook.
- `components/MobileShell.tsx` — compact shell:
  `MobileTopBar` / `NoticeBar` / (list root | detail body) / `MobileTabBar`.
  Chooses list-vs-detail from `selection.kind`.
- `components/MobileTopBar.tsx` — compact replacement for `Titlebar`:
  - list mode: `ProjectSwitcher` (left) · conn dot + Settings gear (right);
  - detail mode: ‹ Back + entity title (left) · Settings gear (right);
  - **no** sidebar-collapse toggle, **no** Windows caption controls, **no**
    macOS traffic-light inset, **no** drag region (all desktop-only).
- `components/MobileTabBar.tsx` — three tabs bound to `ui.sidebarPanel`;
  active-tab highlight; bottom `env(safe-area-inset-bottom)` padding; ≥44px
  tap targets. Tab tap = `setSidebarPanel` + `clearSelection`.

**Changed:**

- `components/AppShell.tsx` — branch at the top:
  `if (useIsCompact()) return <MobileShell/>;` else the current two-pane body.
  This is the only wiring change to the existing shell.
- `App.tsx` — unchanged (still mounts `AppShell`).

**Reused verbatim (no edits, just re-hosted by `MobileShell`):**

- `TasksPanel` / `SessionsPanel` / `WorkflowsPanel` — render only the one
  matching `ui.sidebarPanel`. Their `PanelHeader` (with the ＋ / ↻ actions and
  the self-contained `NewTaskModal` / `NewSessionModal` / `BrowseExtensionsModal`)
  becomes the list screen's section header via compact CSS. The accordion
  "active/collapse" logic is inert when only one panel is shown.
- `CenterPanel` (+ `Composer`, all four views) — the detail body, unchanged.
- All modals (`SettingsModal`, `NewTaskModal`, `NewSessionModal`,
  `BrowseExtensionsModal`, `AddProjectModal`, `ConfirmDialog`) — unchanged JSX;
  restyled to full-screen sheets purely via the compact `@media` block.

## CSS strategy

New stylesheet `gui/src/styles/mobile.css`, imported last in `main.tsx`
(after `modal.css`) so its compact-`@media` rules win cascade ties. Everything
is gated behind the activation media query so desktop is byte-for-byte
unaffected. Contents:

- `.mobile-shell` grid: `topbar / body(1fr) / tabbar`, `height: 100dvh`, with
  `env(safe-area-inset-*)` top/bottom.
- `.mobile-topbar`, `.mobile-tabbar` styling; ≥44px tap targets for tabs,
  rows, and the ＋ / ↻ / Settings buttons (current `btn-ghost` is mouse-sized).
- Make the single hosted `.sidebar-panel` full-height (drop the 3:1 flex
  weighting; the resizer and accordion collapse are not rendered in compact).
- `.composer` spans full width above the safe-area bottom inset on the detail
  screen (no tab bar present there).
- `modal.css` override: under the compact query, `.modal` fills the viewport
  (full-screen sheet) with safe-area insets; keep the × close button (overlay
  tap-to-close still works via the existing `onMouseDown`).

No changes to `ds-tokens.css` values; the compact rules reference existing
tokens (`--titlebar-height`, etc.) and may introduce a couple of compact-only
sizes locally in `mobile.css`.

## Secondary / polish items

- **Keyboard avoidance (iOS):** add `interactive-widget=resizes-content` to the
  `index.html` viewport meta so the composer rides above the on-screen keyboard
  (works with the existing `100dvh`). Low-risk, desktop-neutral.
- **UI zoom:** nothing to do — `isWebviewZoomSupported()` already returns
  `false` on iOS, so no zoom control renders; the Cmd/Ctrl shortcuts are inert
  without a hardware keyboard.
- **Touch targets:** bump list rows and the ＋ / ↻ icon buttons to ≥44px in the
  compact block.

## File-by-file change list

New:
- `gui/src/features/layout/utils/compact.ts`
- `gui/src/features/layout/hooks/useIsCompact.ts`
- `gui/src/features/layout/components/MobileShell.tsx`
- `gui/src/features/layout/components/MobileTopBar.tsx`
- `gui/src/features/layout/components/MobileTabBar.tsx`
- `gui/src/styles/mobile.css`
- `gui/src/features/layout/utils/compact.test.ts` (pure-logic unit test)

Changed:
- `gui/src/features/layout/components/AppShell.tsx` (branch on `useIsCompact`)
- `gui/src/main.tsx` (import `./styles/mobile.css` last)
- `gui/index.html` (viewport `interactive-widget=resizes-content`)
- `docs/gui-release.md` (retitle "iOS / iPad" → cover iPhone + compact UI;
  the build/install commands already work for iPhone — note the layout)
- `gui/README.md` (UI-shell section: mention the compact single-pane layout)
- `CHANGELOG.md` (`## [Unreleased] → ### Added`: iPhone-friendly compact
  single-pane layout with bottom tab navigation — user-visible)

Not changed (verified sufficient as-is):
- `gui/src-tauri/gen/apple/...` — `TARGETED_DEVICE_FAMILY = "1,2"` already
  targets iPhone + iPad; `project.yml` already declares iPhone orientations.
- `scripts/install-gui-ios.sh` — already device-agnostic (auto-detects any
  connected iOS device; copy already says "iPhone/iPad").
- `state/*` — no reducer/selector/effect/RPC changes.

## Testing

- **Pure-logic unit test** (`compact.test.ts`) for `isCompactViewport` across
  the matrix: iPhone portrait (390×844, coarse) → true; iPhone landscape
  (844×390, coarse) → true (height clause); iPad portrait (768×1024, coarse)
  → false; iPad landscape (1024×768, coarse) → false; narrow desktop
  (600×800, fine pointer) → false (3C). Fits the existing `vitest`
  "pure logic, no browser/daemon" harness.
- `npm run typecheck` (tsc + IPC-discipline guard — no new `invoke`/`listen`
  sites are introduced, so the guard stays green).
- Manual on-device validation (per `gui/README.md`'s known limitation that the
  live runtime isn't exercised in CI): build with
  `scripts/install-gui-ios.sh` onto an iPhone; verify tab switching, push/back,
  composer + keyboard, safe-area insets in both orientations, and that an iPad
  build is unchanged (still two-pane).

## Risks & mitigations

- **Breakpoint misfires.** The dual width/height clause is the main subtlety
  (phone landscape). Covered by the unit-test matrix above; the `pointer:
  coarse` gate keeps all desktop windows out (3C).
- **`PanelHeader` reuse.** Reusing the accordion header as a list section header
  relies on CSS only; if it looks off, the fallback is a thin `MobileListScreen`
  wrapper that renders the panel body + a dedicated header — kept out of scope
  unless needed.
- **iPadOS reports as "mac".** Irrelevant here: activation uses `pointer:
  coarse` + size, not platform string, so iPad is correctly handled by the
  size clauses (stays two-pane).

## Suggested rollout order

1. `compact.ts` + test, `useIsCompact.ts`.
2. `MobileTabBar`, `MobileTopBar`, `MobileShell`; branch in `AppShell`.
3. `mobile.css` + import; modal full-screen-sheet rule; `index.html` viewport.
4. Touch-target + safe-area polish; on-device pass.
5. Docs (`gui-release.md`, `gui/README.md`) + `CHANGELOG.md`.
```
