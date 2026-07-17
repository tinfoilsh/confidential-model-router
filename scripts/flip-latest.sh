#!/usr/bin/env bash
set -euo pipefail

# Marks a router release as the latest GitHub release and waits until the
# public github-proxy (the path clients verify routers against) serves it.
# Run immediately before accepting the router that runs this version:
#
#   scripts/flip-latest.sh v0.0.97 && <accept the router>

REPO="${REPO:-tinfoilsh/confidential-model-router}"
VERSION="${1:?usage: flip-latest.sh <version>}"

gh release edit "$VERSION" -R "$REPO" --prerelease=false --latest

echo "waiting for $VERSION to be served as latest..." >&2
for _ in $(seq 45); do
  latest="$(curl -fsS "https://github-proxy.tinfoil.sh/repos/$REPO/releases/latest" | jq -r .tag_name || true)"
  if [ "$latest" = "$VERSION" ]; then
    echo "$VERSION is now latest"
    exit 0
  fi
  sleep 2
done

echo "error: github-proxy still serves ${latest:-nothing} as latest after 90s" >&2
exit 1
