# autosk/pi-runtime — the thin operator image for `dockerSandbox({ image })` with
# the pi harness (the Claude Code twin is docker/claude-runtime).
#
# Contract (docs/workflows.md → dockerSandbox, daemon/extensions/sandbox/README.md):
#   - ships the harness (`pi`, authenticated at RUNTIME via a mounted ~/.pi —
#     NOT baked in) plus the build/test toolchain (Go, Bun, git, make,
#     golangci-lint, Node) and the GitHub CLI (`gh`, for the read-only reviews
#     of @autosk/gh-review — authenticated at RUNTIME via a GH_TOKEN env the
#     workflow injects from the operator's ~/.autosk/github/ro-token.json);
#   - NO socat, NO autosk/autoskd, NO mounted daemon socket — pi reaches the
#     per-session host HTTP MCP server over host.docker.internal via the injected
#     `pi-mcp-extension.ts` (bind-mounted by the agent), so the image is thin;
#   - tolerates an ARBITRARY uid:gid (dockerSandbox runs `--user <host uid:gid>`),
#     so $HOME and the caches are made world-writable.
#
# Multi-arch: build args are pinned but overridable; `TARGETARCH` is provided by
# buildx (falls back to dpkg for a plain `docker build`).

FROM debian:bookworm-slim

ARG GO_VERSION=1.25.0
ARG GOLANGCI_VERSION=2.9.0
ARG BUN_VERSION=1.3.14
ARG NODE_MAJOR=22
# pi is a Node CLI (engines: node >=22.19); pin to the operator's version,
# override with PI_VERSION=latest when you bump the host pi.
ARG PI_VERSION=0.81.1
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

# --- Node.js (pi's runtime) + pi CLI ----------------------------------------
RUN set -eux; \
    curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash -; \
    apt-get install -y --no-install-recommends nodejs; \
    rm -rf /var/lib/apt/lists/*; \
    npm install -g "@earendil-works/pi-coding-agent@${PI_VERSION}"; \
    npm cache clean --force; \
    pi --version

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

# --- GitHub CLI (gh) — read-only reviews (@autosk/gh-review) -----------------
# Auth is NOT baked in and NOT a mounted config file: gh-review injects the
# operator's read-only fine-grained PAT (from ~/.autosk/github/ro-token.json on
# the host) as the container's GH_TOKEN env — gh's native token mechanism (gh
# ≥2.40 rewrites a mounted hosts.yml, so a config-file mount does not work).
RUN set -eux; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg; \
    chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends gh; \
    rm -rf /var/lib/apt/lists/*; \
    gh --version

# --- arbitrary-uid HOME -----------------------------------------------------
# dockerSandbox passes `--user <host uid:gid>` (e.g. macOS 501:20), which need
# not exist in /etc/passwd. Create a default `agent` user for ergonomics, then
# make $HOME + caches world-writable so ANY uid can write config/caches.
RUN set -eux; \
    useradd --create-home --home-dir /home/agent --shell /bin/bash --uid 1000 agent || true; \
    mkdir -p /home/agent/.pi /home/agent/.cache /home/agent/go/bin /home/agent/go/pkg/mod; \
    chmod -R 0777 /home/agent

WORKDIR /home/agent
# A login-ish default; dockerSandbox overrides --user/-w/-e HOME per task.
USER agent
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["bash"]
