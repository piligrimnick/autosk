# @autosk/feature-dev-docker

The Docker variant of [`@autosk/feature-dev`](../feature-dev/README.md): the same
`dev → review → docs → validator → accept (human) → cleanup → done` pi cycle, but
every agent step runs inside a **per-task `docker run -i --rm` container**
(`ghcr.io/wierdbytes/pi-runtime`) instead of on the host.

It reuses `featureDevWorkflow()` verbatim — bounce-backs (`review→dev`,
`validator→dev`), the `dev` visit-cap, and the `cleanup` teardown step — only
swapping the default `worktreeSandbox()` for a credential- and git-aware
`dockerSandbox`. It registers ONE workflow, **`feature-dev-docker`**.

## How it works

- **Thin image, host MCP.** Under a `dockerSandbox` (`sandbox.thin === true`)
  `@autosk/pi-agent` mints a per-session host HTTP MCP server and injects only
  the ack-only `autosk_transit` tool. The transport-aware
  [`@autosk/pi-tools`](../../../pi-tools/README.md) (loaded from the mounted
  `~/.pi`) POSTs `autosk_task`/`autosk_comment` to that server over
  `host.docker.internal` — so the image needs neither `autosk` nor a mounted
  daemon socket.
- **Auth.** pi keeps its provider tokens in `~/.pi/agent/auth.json` (a portable
  file), so the host `~/.pi` is bind-mounted at the container's `/home/agent/.pi`
  (read-write). No export step.
- **Git.** `dockerSandbox` bind-mounts only the per-task git *worktree*; a
  worktree's `.git` points into `<projectRoot>/.git/worktrees/<id>`, so this
  package also bind-mounts the project `.git` at its identical path (layered in
  at `wrap()` time) so in-container `git`/`go`/`make` resolve the repo.

## Use it

```bash
# 1. build (or pull) the pi-runtime image
daemon/extensions/pi-agent/docker/build.sh

# 2. install this extension + restart the daemon
autosk ext add npm:@autosk/feature-dev-docker     # or: autosk ext add /path/to/feature-dev-docker

# 3. enroll a task
autosk enroll <task-id> --workflow feature-dev-docker
```

Env knobs (all optional):

| var | default | what |
|-----|---------|------|
| `AUTOSK_PI_DOCKER_IMAGE` | `ghcr.io/wierdbytes/pi-runtime:latest` | image to run |
| `AUTOSK_PI_DIR` | `~/.pi` | host pi config (auth + models) bind-mounted into the container |

## Exports

- default — the extension factory (registers `feature-dev-docker`).
- `defaultDockerImage()` / `featureDevDockerSandbox()` — for composing your own
  workflow over the same docker sandbox.
