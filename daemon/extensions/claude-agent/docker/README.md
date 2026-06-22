# `claude-runtime` — the docker sandbox image (Claude Code harness)

The thin operator image a `dockerSandbox({ image })` would run the **Claude Code**
harness inside. It carries `claude` plus the autosk build/test toolchain (Go 1.25,
Bun, git, make, golangci-lint, Node) and **nothing else** — no `socat`, no
`autosk`/`autoskd`, no mounted daemon socket. The agent's tool surface is the
per-session **host HTTP MCP server** (`--mcp-config type:"http"`); the container
reaches it over `host.docker.internal` (`dockerSandbox.wrap` adds
`--add-host=host.docker.internal:host-gateway`).

> **The Claude `dockerSandbox` workflow is deferred** (`@autosk/feature-dev-cc`
> ships the host/worktree workflow only). This image is built + published now so
> it is ready when the docker variant lands. Today it is wired by hand:
> `claudeAgent({ sandbox: dockerSandbox({ image: "ghcr.io/wierdbytes/claude-runtime:latest" }) })`.

Auth is **not baked into the image** — it is mounted at run time from your host
Claude credentials (see below).

## Build (local) / Publish (GHCR)

```bash
daemon/extensions/claude-agent/docker/build.sh        # → ghcr.io/wierdbytes/claude-runtime:latest (local)

echo "$GHCR_TOKEN" | docker login ghcr.io -u wierdbytes --password-stdin
scripts/publish-extensions.sh --publish               # npm packages + pi-runtime + claude-runtime
```

Override toolchain versions with build args, e.g. `GO_VERSION=1.25.1 …/build.sh`.
Publishing is multi-arch `buildx` push via
[`scripts/publish-extensions.sh`](../../../../scripts/publish-extensions.sh)
(`$GHCR_TOKEN` needs `write:packages`; skip with `--no-images`).

## Credentials from your system Claude Code

Claude Code on **macOS keeps its OAuth token in the login Keychain** (item
`Claude Code-credentials`), not in `~/.claude/.credentials.json` — so bind-mounting
`~/.claude` alone carries **no token**. `export-claude-credentials.sh` exports the
Keychain payload (already in the `{"claudeAiOauth":{…}}` shape the Linux `claude`
reads) into a 0600 file:

```bash
daemon/extensions/claude-agent/docker/export-claude-credentials.sh
# → ~/.autosk/claude-runtime/.credentials.json (0600)
```

A docker workflow would then mount, per task: host `~/.claude` →
`/home/agent/.claude` (rw), the exported token → `/home/agent/.claude/.credentials.json`
(read-only overlay — this is what authenticates the container `claude`), and the
per-task project `.git` (so the git worktree resolves its commondir). The token
has an expiry and the overlay is read-only; re-run the export after a host
`claude` refresh, or `--clean` to remove the managed file.

> API-key auth instead of a subscription? Skip the export and pass the key as
> container env via `dockerSandbox({ env: { ANTHROPIC_API_KEY } })`.

### Notes

- **Arbitrary UID.** `dockerSandbox` runs `--user <host uid:gid>` so bind-mounted
  files stay host-owned; the image makes `$HOME` + caches world-writable.
