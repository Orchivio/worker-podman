#!/usr/bin/env bash
# Builds the worker and sandbox images, creates the internal sandbox
# network and (re)starts the credential/egress proxy container.
# The proxy is the only container on orchivio-sandbox with a route out.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
worker="$(dirname "$here")"
network=orchivio-sandbox
# The proxy's way out. Not the default "podman" network: that one has no
# DNS, and the internal network's DNS does not resolve external names.
egress=orchivio-egress

make -C "$worker" image
make -C "$worker/../orchivio-adapter-claude-code" image

podman network exists "$network" || podman network create --internal "$network"
podman network exists "$egress" || podman network create "$egress"

# Pass through only the credential that is set; never print it.
cred=()
if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
  cred=(-e ANTHROPIC_API_KEY)
elif [[ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]]; then
  cred=(-e CLAUDE_CODE_OAUTH_TOKEN)
else
  echo "set ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN" >&2
  exit 1
fi

podman rm -f orchivio-proxy >/dev/null
podman run -d --name orchivio-proxy --network "$network" --network "$egress" \
  "${cred[@]}" localhost/orchivio-worker:dev proxy
