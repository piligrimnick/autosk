# Landing website for autosk (`website/`)

**Status:** plan (no code yet — section structure, copy narrative, and stack).
**Date:** 2026-06-24.
**Owners:** autosk web / marketing.

**Related references:**
- `~/me/dev/pi-website` — the Earendil brand site we take *visual/voice cues* from
  (custom Python SSG, hand-written `styles.css`, WebGL hero, literary tone). We
  **do not** copy its architecture — it is a one-section art site with no
  conversion funnel.
- `README.md`, `docs/concepts.md`, `docs/workflows.md`, `docs/daemon.md`,
  `docs/extensions.md`, `docs/lazy.md`, `gui/README.md` — source of truth for the
  feature copy below.
- `docs/hero.png` — existing hero image (TUI + desktop GUI + mobile web view) we
  reuse above the fold.
- `gui/src/styles/base.css`, `gui/src/styles/ds-tokens.css` — the product's dark
  palette + status hues, which the site adopts so the marketing surface matches
  the app.

---

## 1. Decisions (locked)

| # | Decision | Choice |
|---|----------|--------|
| D1 | Stack | **Astro + Tailwind** (static output, island-friendly, component-based) |
| D2 | Narrative format | **Classic conversion funnel**: hero → problem → solution model → how-it-works → feature grid → deep-dives → comparison → trust → CTA → footer |
| D3 | Placement | New **`website/`** directory inside the `autosk` monorepo |
| D4 | This deliverable | **Plan document only** — section structure + copy narrative + stack. No scaffold yet. |
| D5 | Copy language | **English** (matches README/brand; mirrors pi-website's primary locale). i18n left as a post-v1 option (see §10). |
| D6 | Theme | **Dark-first**, reusing the GUI's TokyoNight-ish palette as the canonical brand tokens. |

---

## 2. Goals & non-goals

**Goals**
- A single marketing page that walks a developer from "what is this" to
  "I ran `brew install`" in one scroll.
- Lead with the genuinely novel selling points: **workflows-as-code**,
  **multi-agent handoff with guardrails**, **agent-owned sandboxes**, **live
  observability across three front ends**, **no-database local files**.
- Visually consistent with the product (same dark palette, mono accents) and
  tonally adjacent to the Earendil brand (editorial serif for big statements).
- Fast, static, no runtime backend; deployable to any static host (GitHub Pages /
  Cloudflare Pages / Netlify).

**Non-goals (v1)**
- No blog/CMS, no pricing page, no auth, no email-capture backend (a newsletter
  band can link out; no Worker like pi-website's `api/`).
- No WebGL shader hero (pi-website's signature) — we use the product screenshots,
  short looping screencasts, and animated SVG diagrams instead.
- No docs site migration — docs stay in `docs/` / GitHub for now; the landing
  *links* to them.

---

## 3. Tech stack & tooling

- **Astro 5** — static site generation, `.astro` components, zero-JS by default,
  partial hydration (`client:visible`) only where we animate.
- **Tailwind CSS** (via `@astrojs/tailwind` or the v4 Vite plugin) — utility
  styling; brand tokens exposed as CSS variables + Tailwind theme extension.
- **TypeScript** for any island logic.
- **Content**: copy authored inline in `.astro` section components for v1 (small
  surface). A `src/content/` collection is optional if we split features into
  data (see §6 "FeatureGrid" — a `features.ts` data array is recommended so the
  grid is data-driven).
- **Icons**: `astro-icon` + a small Lucide/Tabler subset (terminal, git-branch,
  boxes, eye, shield, plug, cpu).
- **Fonts**:
  - Mono (UI/code): `"Departure Mono"` (pi-website's mono, brand bridge) with
    `ui-monospace, SFMono-Regular, Menlo` fallback. Self-hosted `woff2`.
  - Sans (body): system stack (`-apple-system, Segoe UI, …`) — matches the GUI.
  - Serif (editorial big statements, optional): `"PlantinNow"`/Georgia fallback,
    reused from pi-website for hero/section pull-quotes to nod at the Earendil
    family. **Optional** — drop if licensing/weight is a concern; system serif is
    fine.
- **Analytics**: optional, privacy-light (Plausible/Umami) — flagged, not in v1.
- **No backend.** Pure static `dist/`.

### Build / dev commands (to live in `website/package.json`)

```bash
cd website
npm install
npm run dev        # astro dev  — localhost:4321
npm run build      # astro build → website/dist/
npm run preview    # serve the built site
```

CI (later, optional): a `website.yml` workflow that runs `npm ci && npm run
build` on PRs touching `website/**`, and deploys `dist/` on `main`.

---

## 4. Directory layout (`website/`)

```
website/
  package.json
  astro.config.mjs
  tailwind.config.mjs            # if Tailwind v3; v4 config can be CSS-first
  tsconfig.json
  public/
    favicon.svg
    og-image.png                 # social card (1200×630)
    fonts/DepartureMono.woff2
    media/                       # screenshots + screencasts (see §8)
      hero.png                   # copied/derived from docs/hero.png
      lazy.png  gui.png  mobile.png
      workflow-graph.svg
      transcript.mp4 / .webm
  src/
    pages/
      index.astro                # the one landing page (composes sections)
    layouts/
      Base.astro                 # <head>, meta/OG, font preload, theme
    components/
      Nav.astro
      Hero.astro
      LogoStrip.astro            # "inspired by / built on" + license band
      Problem.astro
      SolutionModel.astro        # tasks → workflows(code) → agents diagram
      HowItWorks.astro           # 3-step create→enroll→watch + CLI snippet
      FeatureGrid.astro          # data-driven from features.ts
      DeepDive.astro             # reusable alternating image/text block
      Comparison.astro           # "why not just an agent / Jira"
      TrustBand.astro            # no-DB, local files, parks-not-crashes, OSS
      FinalCTA.astro
      Footer.astro
      ui/
        Button.astro
        Card.astro
        Section.astro            # max-width + vertical rhythm wrapper
        CodeBlock.astro          # mono, copy-button, prompt styling
        Badge.astro              # status pill, reuses status hues
        Diagram.astro            # inline SVG wrapper w/ optional animation
    data/
      features.ts                # the FeatureGrid source array
      nav.ts
    styles/
      tokens.css                 # brand CSS variables (see §5)
      global.css
```

---

## 5. Design system / brand tokens

Adopt the GUI palette verbatim so the marketing site reads as "the same product."
Source: `gui/src/styles/base.css` + `ds-tokens.css`.

```css
:root {
  /* surfaces (dark-first) */
  --bg:    #16181d;  --bg-1: #1c1f26;  --bg-2: #22262e;  --bg-3: #2a2f39;
  --border:#333a45;
  /* text */
  --fg:    #e6e9ef;  --fg-dim:#9aa3b0; --fg-mute:#828b9c;
  /* brand accents */
  --accent:#4c8dff;  --accent-2:#6ee7b7;
  --danger:#ef5350;  --warn:#f2c14e;   --ok:#4ade80;
  /* status hues (status machine: new/work/human/done/cancel) */
  --status-new:#8aa0c6; --status-work:#4c8dff; --status-human:#f2c14e;
  --status-done:#4ade80; --status-cancel:#6b7280;
  /* entity hues (reused in diagrams/code: task/session/workflow/step/agent) */
  --task-id:#7aa2f7; --session-id:#bb9af7; --workflow-name:#e0af68;
  --step-name:#9d7cd8; --agent-name:#7dcfff;
  --radius:8px;
}
```

Usage rules:
- **Accent blue** (`--accent`) = primary CTAs, links, the "work" status.
- **Mint** (`--accent-2`) = success/"it just works" highlights, secondary accent.
- **Status hues** = used in the workflow diagram and any pill that mirrors a real
  task status, so the visuals teach the model.
- Code/terminal blocks use the mono font on `--bg` with `--accent`/`--accent-2`
  prompt + entity coloring (mirrors lazy mode's TokyoNight scheme).
- Generous whitespace, large editorial headings (serif optional), short mono
  eyebrows/kickers above each section heading.

---

## 6. Landing page section structure (the narrative — the core of this plan)

Single scroll, top → bottom. Each block lists: **purpose**, **draft copy**
(verbatim, editable), and **visual**. Copy is intentionally punchy/scan-friendly
per the chosen funnel format.

### 0. Nav (sticky, minimal)
- Left: `autosk` wordmark (mono) + small logo (`gui/src-tauri/icons/src/autosk.png`).
- Center/right links: **Features · How it works · Docs · GitHub**.
- Right CTA button: **Install** (scrolls to Final CTA / copies brew command).

### 1. Hero (above the fold)
**Purpose:** one-sentence promise + the "three front ends" image + an instant
proof-of-simplicity terminal snippet + dual CTA.

- **Eyebrow (mono):** `task manager + workflow engine for coding agents`
- **Headline (H1):** **You don't need a smarter agent. You need a way to run them in a loop.**
- **Subhead:** Local-first task tracking and code-defined workflows that drive
  your coding agents — Claude Code, pi, or your own — through a real pipeline you
  can watch, steer, and trust.
- **Primary CTA:** `Install for macOS` → reveals/copies
  `brew install --cask wierdbytes/autosk/autosk`
- **Secondary CTA:** `View on GitHub` (+ a `★` count if we wire it later).
- **Hero visual:** `docs/hero.png` (the lazy TUI, the desktop GUI, the mobile web
  view side by side).
- **Inline terminal proof (CodeBlock):**
  ```bash
  $ autosk create "Wire up the auth flow"
  ask-3f9b2c
  $ autosk enroll ask-3f9b2c --workflow feature-dev
  # the daemon runs the agent pipeline and returns when done or parked
  ```

### 2. Logo / credibility strip (thin band)
**Purpose:** instant trust without a wall of logos.
- Line 1 (mono, muted): `Open source · MIT · macOS · Linux · iOS`
- Optional: `signed + notarized` badge for the macOS cask.

### 3. The problem
**Purpose:** name the pain of unmanaged agents.
- **Eyebrow:** `the problem`
- **Heading:** **One chat window is not a process.**
- **Body:** Coding agents are powerful but unmanaged. One context, no memory of
  "what's next," no handoff from a coder to a reviewer, no record of why work
  parked. So you re-prompt, copy-paste context between tabs, and hope the agent
  transitions cleanly.
- **Visual:** a "before" sketch — a single chaotic chat bubble vs. the ordered
  pipeline revealed in the next section. 3 pain bullets:
  - *No handoffs* — the coder and the reviewer are the same forgetful chat.
  - *No guardrails* — nothing stops an agent looping on the same failing step.
  - *No memory* — context dies when the tab closes; nothing lands anywhere.

### 4. The solution model
**Purpose:** the one-sentence mental model + a diagram that teaches it.
- **Eyebrow:** `the model`
- **Heading:** **Tasks → workflows (as code) → agents the daemon drives.**
- **Body:** autosk gives agent work *structure* (tasks with dependencies),
  *process* (workflows written in TypeScript, versioned in your repo), and
  *observability* (live transcripts) — while everything stays local and
  file-based under `.autosk/`.
- **Visual — `Diagram.astro`:** animated horizontal flow using the real
  `feature-dev` graph and status/entity hues:
  `dev → review → docs → validator → accept → cleanup → done`, with `review` and
  `validator` drawing a "bounce-back" arrow to `dev`. Each node labeled with its
  owning agent.

### 5. How it works (30-second flow)
**Purpose:** prove how little ceremony there is.
- **Eyebrow:** `30 seconds`
- **Heading:** **Create. Enroll. Watch.**
- **Three numbered steps**, each a short line + a one-line command:
  1. **Create a task** — `autosk create "…"` → `ask-3f9b2c`
  2. **Enroll into a workflow** — `autosk enroll ask-3f9b2c --workflow feature-dev`
  3. **Watch it run** — open `autosk lazy` (or the GUI) and the agent's
     transcript streams live into the detail pane — text, thinking, tool calls —
     and you can steer or abort mid-turn.
- **Visual:** a short looping screencast of the lazy TUI streaming a transcript
  (`media/transcript.webm`), poster = `media/lazy.png`.

### 6. Feature grid (the selling features)
**Purpose:** the scannable proof of depth. Data-driven from `src/data/features.ts`
so we can reorder freely. ~8 cards, lead with the novel ones. Each card = icon +
3-5 word title + one benefit sentence.

1. **Workflows as code, not config** — A workflow is a TypeScript graph of steps,
   each owned by an agent, versioned in your repo. No DB, no brittle JSON editor.
2. **Multi-agent handoffs** — Agents pass work through comments; every prior
   comment is injected at the top of the next step's prompt, so context carries.
3. **Guardrails that stop loops** — `step_visits` caps how many times a task can
   bounce through a step; after N failed reviews it parks for a human, not forever.
4. **Never lose work** — Any run that can't transition cleanly is parked to
   `human`. Crash recovery seals interrupted sessions so work always lands
   somewhere a person can pick it up.
5. **Agent-owned sandboxes** — Run each task in its own git worktree
   (`autosk/<task-id>`) or a per-task Docker container, so parallel agents never
   clobber each other's files.
6. **No database — just files** — Tasks, comments, and transcripts are plain
   files under `.autosk/`. Hand-editable, git-diffable, no migrator, no engine.
7. **Three front ends, one source of truth** — A lazygit-style TUI, a native
   Tauri desktop GUI (+ iPhone/iPad), and the CLI — all pure JSON-RPC clients of
   one daemon, all streaming the same live transcript.
8. **Bring your own agent** — Ships `pi-agent` (drives `pi --mode rpc`) and
   `claude-agent` (drives Claude Code headless). Swap them inline in a workflow,
   or write your own.
9. *(optional 9th)* **Extensions, no build step** — Drop a `.ts` file in
   `.autosk/extensions/` or `autosk ext add npm:@scope/pkg`; a broken extension
   shows up in diagnostics instead of crashing the daemon.

### 7. Deep-dive blocks (3-4 alternating image/text)
**Purpose:** give the flagship features room to breathe with a real screenshot.
Reusable `DeepDive.astro` (heading + body + bullet list + media, image side
alternates).

- **DD-1 — Workflows you can read in a diff.** Show a `feature-dev` step graph +
  a TypeScript snippet of a step. Selling point: process lives in code review,
  evolves with the repo, no hidden state.
- **DD-2 — Handoffs with a human escape hatch.** Show comments threading between
  `dev` and `review`, and a task parked in `human` with its `step_visits`.
  Selling point: agents collaborate, but a person is always the backstop.
- **DD-3 — Isolation without orchestration glue.** Show a worktree branch per
  task / a Docker run. Selling point: true parallelism; the engine knows nothing
  about sandboxes, agents own them.
- **DD-4 — Watch the work, not just the result.** Show the GUI/TUI live
  transcript (thinking + tool calls), steer/abort. Selling point: observability
  you don't get from a headless one-shot.

### 8. Comparison — "Why not just…"
**Purpose:** disarm the two obvious objections. Two-column or small table.
- **vs. using an agent directly:** you get a *pipeline* (multi-step, multi-role,
  with handoffs + guardrails), *parallelism* (per-task sandboxes), and
  *observability* (live + persistent transcripts) — instead of one ephemeral chat.
- **vs. a task tracker (Jira/Linear):** autosk isn't a ticket board, it's a
  *runtime*. Tasks aren't tickets — they're units the engine actually executes
  through agents. (And you can still stop at step 1 and use it as a plain backlog.)

### 9. Trust band
**Purpose:** consolidate the "this is safe and yours" message.
- **Heading:** **Local, file-based, and open.**
- Four compact stats/claims:
  - *No database.* Everything is files in your repo.
  - *Parks, never crashes.* Interrupted work always lands in `human`.
  - *Open source, MIT.* Yours to read, fork, and run.
  - *Signed & notarized.* macOS cask; Linux binaries + AppImage/.deb; iOS TestFlight.

### 10. Final CTA
**Purpose:** the conversion. Big, centered, terminal-styled.
- **Heading:** **Run your agents in a loop.**
- **Install block (tabs: macOS / Linux / source):**
  - macOS: `brew install --cask wierdbytes/autosk/autosk`
  - Linux: link to latest GitHub Release (binaries + AppImage/.deb)
  - source: `make install`
- **Then:** `autosk lazy`
- Secondary links: Docs · GitHub · Changelog.

### 11. Footer
- Columns: **Product** (Features, How it works, Download) · **Docs** (Concepts,
  Workflows, Daemon, Extensions, Lazy) · **Project** (GitHub, Releases,
  Changelog, License) · **Brand** (Earendil link).
- Bottom line: `© Earendil Inc. · MIT · autosk` + theme toggle (optional).

---

## 7. Component inventory (build order)

| Component | Notes |
|-----------|-------|
| `Section.astro` | Max-width (≈1120px), vertical rhythm, eyebrow + heading slots |
| `Button.astro` | Variants: primary (accent), ghost; supports "copy command" mode |
| `CodeBlock.astro` | Mono, prompt styling, copy button, entity/status coloring |
| `Card.astro` | Feature card: icon, title, body |
| `Badge.astro` | Status/entity pill reusing brand hues |
| `Diagram.astro` | Inline SVG; the workflow graph + the solution-model flow |
| `DeepDive.astro` | Alternating image/text block (props: side, media, bullets) |
| `Nav.astro` / `Footer.astro` | Chrome |
| Section comps | `Hero, LogoStrip, Problem, SolutionModel, HowItWorks, FeatureGrid, Comparison, TrustBand, FinalCTA` |

Hydration: only `CodeBlock` (copy button), the screencast players, and any
animated diagram need `client:visible`; everything else ships as static HTML.

---

## 8. Assets to produce

- **Reuse:** `docs/hero.png` (hero), `gui/src-tauri/icons/src/autosk.png` (logo /
  favicon source).
- **New screenshots:** `lazy.png` (TUI streaming a transcript), `gui.png`
  (desktop detail pane), `mobile.png` (iPhone compact), parked-task with
  `step_visits`, a `.ts` step snippet (carbon-style or real editor).
- **New screencasts (short, looping, muted):** lazy transcript stream
  (`transcript.webm`), an end-to-end `create → enroll → watch` run.
- **Diagrams (SVG):** the `feature-dev` graph with bounce-backs; the
  tasks→workflows→agents model.
- **Social:** `og-image.png` (1200×630), `favicon.svg`.
- **Font:** self-host `DepartureMono.woff2` (verify license) or fall back to
  system mono.

---

## 9. Implementation phases (when we build)

1. **Scaffold** — `npm create astro@latest website`, add Tailwind, wire
   `tokens.css` + fonts + `Base.astro` (meta/OG), commit empty section stubs.
2. **Chrome + Hero** — Nav, Hero (with `docs/hero.png` + terminal snippet), Footer,
   FinalCTA. Site is shippable as a one-screen teaser at this point.
3. **Narrative core** — Problem, SolutionModel (diagram), HowItWorks. The story
   reads end to end.
4. **FeatureGrid** — `features.ts` + cards; the scannable depth.
5. **Deep-dives + Comparison + TrustBand** — once real screenshots/screencasts
   exist.
6. **Polish** — responsive passes (mobile/tablet), motion (reduced-motion safe),
   Lighthouse/perf, OG card, 404.
7. **Deploy** — pick host (see §10), add `website.yml` CI (build on PR, deploy on
   `main`).

A v1-MVP is phases 1-4; deep-dives/comparison/trust (5) are the "depth" upgrade.

---

## 10. Open questions

- **Hosting/domain.** GitHub Pages vs Cloudflare Pages vs Netlify? Under an
  `earendil` domain (e.g. `autosk.earendil.com`) or a standalone `autosk.dev`?
  → affects `astro.config.mjs` `site`/`base` and the CI deploy step.
- **i18n.** English-only for v1 (locked, D5). Do we want a Russian (or pi-website-
  style multi-locale) pass later? Astro i18n routing is cheap to add if so.
- **Editorial serif.** Use pi-website's `PlantinNow` for big statements (brand
  bridge to Earendil) or keep it all mono+sans? Licensing/weight check needed.
- **Screencasts vs static.** Do we invest in looping screencasts for v1, or ship
  static screenshots first and add motion in phase 6?
- **GitHub star count / live badges.** Static at build time, or a tiny client
  fetch? (Prefer build-time to keep it zero-JS.)
- **Newsletter/email capture.** Out of scope for v1 (no backend). Link to GitHub
  "watch/releases" instead, or add a Cloudflare Worker later like pi-website.
- **Relationship to pi-website.** Should this eventually live as a section/sibling
  under the Earendil site rather than a standalone? For now: standalone in
  `website/`.

---

## 11. Summary

Build a fast, static **Astro + Tailwind** landing page in `website/`, dark-themed
with the product's own palette, that tells a linear conversion story:
**problem (one chat ≠ a process) → model (tasks → workflows-as-code → agents) →
how-it-works (create/enroll/watch) → feature grid → flagship deep-dives →
"why not just an agent/Jira" → trust (local, file-based, open) → install CTA.**
The selling features lead with what is genuinely novel about autosk —
workflows-as-code, multi-agent handoffs with `step_visits` guardrails,
agent-owned worktree/Docker sandboxes, live observability across three front ends,
and a no-database local file model. This document is the section map + draft copy;
the next step (separate PR) is the Astro scaffold per §9.
