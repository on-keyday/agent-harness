#!/usr/bin/env bash
# Build the agent sandbox image used by agent-in-podman.sh.
#
#   scripts/sandbox/build.sh                       # :latest, claude latest
#   scripts/sandbox/build.sh --build-arg CLAUDE_VERSION=2.1.169
#   HARNESS_SANDBOX_IMAGE=foo:dev scripts/sandbox/build.sh
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$DIR/../.." && pwd)"
IMAGE="${HARNESS_SANDBOX_IMAGE:-harness-agent-sandbox:latest}"

# The CONNECT proxy is Go (cmd/sandbox-connect-proxy) so it lives under the
# repo's go vet / go test, unlike the shell and python pieces around it. Build
# it into the context here rather than in a builder stage: the toolchain is
# already a hard requirement of this repo, and a golang builder image would add
# ~800MB of pull to a build whose whole point is one 3MB static binary.
#
# CGO_ENABLED=0 because the image has no libc guarantee for the proxy's own
# uid; GOARCH is left at the host's, which is what podman builds for here.
echo "building sandbox-connect-proxy for the image..." >&2
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
  -o "$DIR/sandbox-connect-proxy" "$REPO/cmd/sandbox-connect-proxy"
trap 'rm -f "$DIR/sandbox-connect-proxy"' EXIT

podman build -t "$IMAGE" "$@" -f "$DIR/Containerfile" "$DIR"
