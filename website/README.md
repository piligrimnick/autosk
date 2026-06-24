# autosk website

The marketing landing page for **autosk** — a task manager + workflow engine for
coding agents. A fast, static, dark-first **Astro + Tailwind** site with no
backend; the build emits a plain `dist/` deployable to any static host.

Design plan: [`docs/plans/20260624-Landing-Website.md`](../docs/plans/20260624-Landing-Website.md).

## Stack

- **[Astro](https://astro.build)** — static site generation, zero JS by default.
- **[Tailwind CSS v4](https://tailwindcss.com)** — via the `@tailwindcss/vite`
  plugin; brand tokens are bridged into the Tailwind theme in
  `src/styles/global.css`.
- **TypeScript** for the small amount of island logic (copy buttons,
  reveal-on-scroll), both gated as progressive enhancement.

The site reuses the desktop GUI's palette verbatim (`src/styles/tokens.css`, a
copy of `gui/src/styles/base.css` tokens) so the marketing surface matches the
app. There is **no self-hosted display font** — it uses the system mono/sans
stacks the GUI uses.

## Commands

Run everything from this `website/` directory. Requires Node.js 18+.

```bash
npm install        # install dependencies
npm run dev        # astro dev — http://localhost:4321
npm run build      # astro build → website/dist/
npm run preview    # serve the built site locally
npm run check      # astro check (type/diagnostics)
```

## Structure

```
website/
  public/
    favicon.svg
    media/hero.png            # reused from docs/hero.png
  src/
    pages/
      index.astro             # the single-scroll landing page
      404.astro
    layouts/Base.astro        # <head>, meta/OG, copy + reveal scripts
    components/
      Nav · Hero · Problem · SolutionModel · HowItWorks
      FeatureGrid · DeepDives (DeepDive) · Comparison · TrustBand
      FinalCTA · Footer
      ui/  Section · Button · Card · Badge · CodeBlock · Diagram · Icon
    data/
      features.ts             # FeatureGrid source (data-driven)
      nav.ts                  # nav links + install command + repo URLs
    styles/
      tokens.css              # brand CSS variables (plan §5)
      global.css              # Tailwind import + theme bridge + base styles
```

## Editing content

- **Features** — edit `src/data/features.ts`; the grid renders straight from the
  array, so reordering or adding a card needs no markup change.
- **Nav / footer links + the brew command** — `src/data/nav.ts`.
- **Brand colors** — `src/styles/tokens.css` (kept in sync with the GUI).
- **Section copy** — inline in each `src/components/*.astro` section.

## Deploy

The output is a static `dist/`. Point any static host (GitHub Pages, Cloudflare
Pages, Netlify) at `npm run build`. Set the production origin in
`astro.config.mjs` (`site`) once the hosting/domain decision is made
(plan §10); it only affects absolute canonical/OG URLs.
