# `pi-runtime` — the docker sandbox image (pi harness)

The thin operator image `dockerSandbox({ image })` runs the **pi** harness inside
(used by [`@autosk/feature-dev-docker`](../../feature-dev-docker/README.md)). It
carries `pi` plus the autosk build/test toolchain (Go 1.25, Bun, git, make,
golangci-lint, Node) and **nothing else** — no `socat`, no `autosk`/`autoskd`, no
mounted daemon socket.

pi runs **thin**: under a `dockerSandbox` (`sandbox.thin === true`) `@autosk/pi-agent`
mints the per-session **host HTTP MCP server**, injects the ack-only
`autosk_transit` tool, and sets `AUTOSK_MCP_URL`/`AUTOSK_MCP_TOKEN` so the
transport-aware [`@autosk/pi-tools`](../../../../pi-tools/README.md) (loaded from
the mounted `~/.pi`) POSTs `autosk_task`/`autosk_comment` to it. The container
reaches the server over `host.docker.internal` (`dockerSandbox.wrap` adds
`--add-host=host.docker.internal:host-gateway`), so no `autosk` CLI or socket is
needed in the image.

Auth is **not baked into the image** — it is mounted at run time from your host
`~/.pi`.

## Build (local)

```bash
daemon/extensions/pi-agent/docker/build.sh            # → ghcr.io/wierdbytes/pi-runtime:latest (local)
PI_VERSION=latest daemon/extensions/pi-agent/docker/build.sh   # track the current host pi
```

The default `PI_VERSION` is pinned (matching the host pi). Override any toolchain
version with build args, e.g. `GO_VERSION=1.25.1 …/build.sh`.

## Publish (GHCR)

Publishing is done by [`scripts/publish-extensions.sh`](../../../../scripts/publish-extensions.sh)
(multi-arch `buildx` push), alongside the npm packages:

```bash
echo "$GHCR_TOKEN" | docker login ghcr.io -u wierdbytes --password-stdin
scripts/publish-extensions.sh --publish            # npm packages + pi-runtime + claude-runtime
```

`$GHCR_TOKEN` needs `write:packages`. Skip images with `--no-images`.

> CI publishes these automatically: on every **green push to `main`**,
> [`.github/workflows/publish.yml`](../../../../.github/workflows/publish.yml)
> runs the same script (GHCR login via the built-in `GITHUB_TOKEN`), tagging the
> images `latest` + `sha-<short>`. The command above is only for out-of-band
> pushes.

## Credentials from your system pi

pi keeps its provider tokens in a plain file — `~/.pi/agent/auth.json`
(anthropic / openai / google / …) — which is host-agnostic, so
`feature-dev-docker` simply bind-mounts the whole host `~/.pi` at `/home/agent/.pi`
(read-write). There is **no export step**.

Per task it mounts:

- host `~/.pi` → `/home/agent/.pi` (rw — `auth.json`, `models.json`, `settings.json`, extensions, skills);
- the per-task project `.git` → its identical host path (so the git worktree resolves its commondir).

> Host-only pi extensions (voice / statusline) that shell out to macOS binaries
> may warn under Linux; `pi --mode rpc` is headless so they are typically inert.
> If one misbehaves, point `AUTOSK_PI_DIR` at a trimmed copy of `~/.pi`.

## Use it

```bash
daemon/extensions/pi-agent/docker/build.sh           # build (or pull) the image
autosk ext add npm:@autosk/feature-dev-docker        # or a local checkout, then restart the daemon
autosk enroll <task-id> --workflow feature-dev-docker
```

Point it at a different image / pi config with `AUTOSK_PI_DOCKER_IMAGE`,
`AUTOSK_PI_DIR`.

### Notes

- **Arbitrary UID.** `dockerSandbox` runs `--user <host uid:gid>` so bind-mounted
  files stay host-owned; the image makes `$HOME` + caches world-writable.
