# autoskd v2 — Bun/TypeScript workspace

This directory hosts the v2 rewrite of the autosk daemon as a Bun + TypeScript
workspace. It replaces the Rust `autoskd` / `autosk-core` / `autosk-proto`
stack (and doltlite) once parity lands; see
`docs/plans/20260612-Bun-Daemon-Extensions.md` for the full design.

## Packages

- **`sdk/`** — `@autosk/sdk`: the public, extension-facing types
  (Task / Session / Workflow / Agent / Isolation / `AutoskAPI`), the
  pi-format transcript entry types, and the proto-v2 wire types. The
  proto-v2 types are the single source of truth that the Go and Tauri
  clients mirror (P7/P8).
- **`core/`** — `@autosk/core`: the daemon binary (file store, project
  manager, extension loader, scheduler, RPC server). Placeholder in P1.
- **`extensions/pi-agent/`** — `@autosk/pi-agent`: the shipped extension that
  drives `pi --mode rpc` as an autosk agent. Placeholder in P1 (built in P6).

## Scripts

Run from this directory:

- `bun install` — install + symlink the workspace packages.
- `bun test` — run every package's `*.test.ts` (pure unit tests, no daemon).
- `bun run typecheck` — `tsc --noEmit` across all workspace packages.
