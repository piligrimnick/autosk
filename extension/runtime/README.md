# @autosk/agent-runtime

Node bootstrapper that the [`autosk`](../../README.md) daemon spawns to
run custom-runner agent packages.

You do not import this package from your code. It is installed
automatically by `autosk agent install` into the global agents prefix
(`~/.autosk/packages/`), and the daemon (`autoskd`) shells it whenever a
workflow step is owned by an agent whose `package.json` declares an
`autosk.agent.runner` path.

## Wire protocol

The autosk daemon executor (`autoskd`, Rust) spawns this binary as:

```
node --import tsx \
     <prefix>/node_modules/@autosk/agent-runtime/dist/bootstrap.js \
     --pkg <npm-name> --runner <abs-path-to-runner-module>
```

A single JSON `RunContextSeed` is written to stdin. The bootstrapper
imports the runner module, constructs a `RunContext` (with `cli`,
`stepNext`, `spawnPi` helpers — see `@autosk/agent-sdk`), and awaits
the runner's default export.

The executor watches `step_signals` after the bootstrapper exits — the
runner is expected to call `ctx.stepNext(...)` exactly once before
returning. Custom runners are single-shot; there is no kickback. A
missed signal fails the run with `agent_did_not_emit_transition`.

See [`docs/plans/20260518-Agent-Packages.md`](../../docs/plans/20260518-Agent-Packages.md)
for the full design and rationale.

## Build

```bash
npm install
npm run build   # → dist/bootstrap.js
```

The `dist/` artefact is what ships in the published package and what
the autosk daemon actually invokes. `--import tsx` lets runner authors
write their `runner` entry points in `.ts` directly without a build
step.

## License

MIT.
