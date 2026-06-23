#!/usr/bin/env bash
# Build the autosk/claude-runtime image LOCALLY (single-arch, loaded into the
# local docker engine). Use ./publish.sh to push a multi-arch image to GHCR.
#
# Env knobs (all optional):
#   IMAGE            target repo            (default ghcr.io/wierdbytes/claude-runtime)
#   TAG              image tag             (default latest)
#   GO_VERSION       Go toolchain          (default 1.25.0)
#   GOLANGCI_VERSION golangci-lint         (default 2.9.0)
#   BUN_VERSION      Bun                   (default 1.3.14)
#   NODE_MAJOR       Node.js major         (default 22)
#   DOCKER           docker binary         (default docker; honours podman)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

IMAGE="${IMAGE:-ghcr.io/wierdbytes/claude-runtime}"
TAG="${TAG:-latest}"
DOCKER="${DOCKER:-docker}"

echo ">> building ${IMAGE}:${TAG} (local, single-arch)"
"$DOCKER" build \
  --build-arg GO_VERSION="${GO_VERSION:-1.25.0}" \
  --build-arg GOLANGCI_VERSION="${GOLANGCI_VERSION:-2.9.0}" \
  --build-arg BUN_VERSION="${BUN_VERSION:-1.3.14}" \
  --build-arg NODE_MAJOR="${NODE_MAJOR:-22}" \
  -t "${IMAGE}:${TAG}" \
  "$here"

echo ">> built ${IMAGE}:${TAG}"
echo ">> the claude dockerSandbox workflow is deferred; this image is published for when it lands."
echo ">> a claudeAgent({ sandbox: dockerSandbox({ image: '${IMAGE}:${TAG}' }) }) step would run in it."
