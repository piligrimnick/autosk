# autosk/claude-runtime — the thin operator image for `dockerSandbox({ image })`.
#
# Contract (docs/workflows.md → dockerSandbox, daemon/extensions/sandbox/README.md):
#   - ships the harness (`claude`, authenticated at RUNTIME via a mounted
#     ~/.claude / .credentials.json — NOT baked in) plus the build/test toolchain
#     (Go, Bun, git, make, golangci-lint, node);
#   - NO socat, NO autosk/autoskd, NO mounted daemon socket — the agent's tool
#     surface is the per-session host HTTP MCP server reached over
#     host.docker.internal (the engine mints it; `wrap()` adds --add-host);
#   - tolerates an ARBITRARY uid:gid (dockerSandbox runs `--user <host uid:gid>`
#     so bind-mounted files stay host-owned), so $HOME and the caches are made
#     world-writable.
#
# Multi-arch: build args are pinned but overridable; `TARGETARCH` is provided by
# buildx (falls back to dpkg for a plain `docker build`).

FROM debian:bookworm-slim

ARG GO_VERSION=1.25.0
ARG GOLANGCI_VERSION=2.9.0
ARG BUN_VERSION=1.3.14
ARG NODE_MAJOR=22
# buildx sets this; default via dpkg for a plain `docker build`.
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/home/agent \
    GOPATH=/home/agent/go \
    GOCACHE=/home/agent/.cache/go-build \
    GOMODCACHE=/home/agent/go/pkg/mod \
    BUN_INSTALL=/usr/local/bun \
    PATH=/usr/local/go/bin:/usr/local/bun/bin:/home/agent/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# --- base toolchain ---------------------------------------------------------
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates curl git make build-essential ripgrep jq xz-utils unzip tini openssh-client; \
    rm -rf /var/lib/apt/lists/*

# --- Node.js (for Claude Code) + Claude Code CLI ----------------------------
RUN set -eux; \
    curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash -; \
    apt-get install -y --no-install-recommends nodejs; \
    rm -rf /var/lib/apt/lists/*; \
    npm install -g @anthropic-ai/claude-code; \
    npm cache clean --force; \
    claude --version

# --- Go ---------------------------------------------------------------------
RUN set -eux; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o /tmp/go.tgz; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm /tmp/go.tgz; \
    /usr/local/go/bin/go version

# --- Bun --------------------------------------------------------------------
RUN set -eux; \
    curl -fsSL https://bun.sh/install | bash -s "bun-v${BUN_VERSION}"; \
    ln -sf /usr/local/bun/bin/bun /usr/local/bin/bun; \
    bun --version

# --- golangci-lint ----------------------------------------------------------
RUN set -eux; \
    curl -fsSL "https://raw.githubusercontent.com/golangci/golangci-lint/v${GOLANGCI_VERSION}/install.sh" \
      | sh -s -- -b /usr/local/bin "v${GOLANGCI_VERSION}"; \
    golangci-lint version

# --- arbitrary-uid HOME -----------------------------------------------------
# dockerSandbox passes `--user <host uid:gid>` (e.g. macOS 501:20), which need
# not exist in /etc/passwd. Create a default `agent` user for ergonomics, then
# make $HOME + caches world-writable so ANY uid can write config/caches.
RUN set -eux; \
    useradd --create-home --home-dir /home/agent --shell /bin/bash --uid 1000 agent || true; \
    mkdir -p /home/agent/.claude /home/agent/.cache /home/agent/go/bin /home/agent/go/pkg/mod; \
    chmod -R 0777 /home/agent

WORKDIR /home/agent
# A login-ish default; dockerSandbox overrides --user/-w/-e HOME per task.
USER agent
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["bash"]
