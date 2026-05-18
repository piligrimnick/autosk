# `@autosk/example-custom-agent`

A minimum-viable custom-runner agent package. Demonstrates:

- `package.json#autosk.agent.runner` pointing at a TS file.
- A default-export `RunAgent` that does one side effect
  (`autosk comment add`) and then calls `ctx.stepNext("done")`.

## Try it locally

The package isn't published. To use it from a checkout:

```bash
# 1. Bring up a fresh autosk project.
mkdir /tmp/autosk-example && cd /tmp/autosk-example
autosk init

# 2. Hand-install this package into the global prefix. Real users will
#    do `autosk agent install @autosk/example-custom-agent` once it's
#    published; for local dev we copy.
mkdir -p ~/.autosk/packages/node_modules/@autosk
cp -r .../docs/notes/agent-package-example \
      ~/.autosk/packages/node_modules/@autosk/example-custom-agent

# 3. Make sure @autosk/agent-runtime is installed and built:
#    cd .../extension/runtime && npm install && npm run build
#    cp -r .../extension/runtime ~/.autosk/packages/node_modules/@autosk/agent-runtime

# 4. Register the agent in autosk's registry by editing
#    ~/.autosk/packages/registry.json (or run a manual `autosk agent install`
#    against a tarball if you have one).

# 5. Use it.
autosk create "say hi" --agent @autosk/example-custom-agent
autosk daemon serve --workers 1 &
# → the example-custom-agent runs, adds a comment, then closes the task.
```

Plan: [`../../plans/20260518-Agent-Packages.md`](../../plans/20260518-Agent-Packages.md).
SDK: [`../../../extension/sdk/`](../../../extension/sdk/).
Runtime / bootstrapper: [`../../../extension/runtime/`](../../../extension/runtime/).
